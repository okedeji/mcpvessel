// Validation cage.
//
// The orchestrator spawns one of these per candidate finding from the
// exploitation phase. The job: re-run the exploitation tool that
// originally detected the issue, against the same path, and report
// whether the re-test reproduced the indicator.
//
// Output: a child finding with kind=validation_proof and
// parentFindingId pointing at the candidate. The findings coordinator
// promotes the parent to Validated when this child carries non-Info
// severity (re-test reproduced), or Rejected when it comes back clean
// (severity Info).
//
// This is the same-tool re-test strategy: catches network flakes,
// timing-dependent results, and probes that depend on session state
// the original cage had but a fresh validation cage doesn't. It does
// NOT catch "the tool's detection logic is wrong" — that needs
// per-vuln-class dedicated validators.

import { FindingKind, Severity, newFindingId } from '@agentcage/sdk';
import { agent } from '../lib/sdk';
import { env } from '../lib/env';
import { Tool, ToolResult } from '../tools/exploitation/types';

import { sqli } from '../tools/exploitation/sqli';
import { xss } from '../tools/exploitation/xss';
import { infoDisclosure } from '../tools/exploitation/info-disclosure';
import { headers } from '../tools/exploitation/headers';
import { cors } from '../tools/exploitation/cors';
import { authBypass } from '../tools/exploitation/auth-bypass';
import { idor } from '../tools/exploitation/idor';
import { csrf } from '../tools/exploitation/csrf';
import { brokenAuth } from '../tools/exploitation/broken-auth';
import { rateLimit } from '../tools/exploitation/rate-limit';

const TOOLS: Tool[] = [
  sqli,
  xss,
  infoDisclosure,
  headers,
  cors,
  authBypass,
  idor,
  csrf,
  brokenAuth,
  rateLimit,
];
const TOOLS_BY_NAME = new Map(TOOLS.map((t) => [t.name, t]));

export async function runValidation(): Promise<void> {
  console.log(`\n── Validation: vuln_class="${env.vulnClass}" parent=${env.parentFindingId} ──`);

  if (!env.parentFindingId) {
    console.log('AGENTCAGE_PARENT_FINDING_ID not set — refusing to validate without a candidate to attach to.');
    return;
  }

  const tool = TOOLS_BY_NAME.get(env.vulnClass);
  if (!tool) {
    await submitProof({
      detected: false,
      severity: Severity.Info,
      title: `No validator for vuln_class="${env.vulnClass}"`,
      description: `The validation cage has no tool registered for vuln_class="${env.vulnClass}". Treating the candidate as unconfirmable.`,
      request: '',
      response: '',
      poc: '',
      reproductionSteps: '',
    });
    return;
  }

  const probePath = env.scopePaths[0] ?? '/';
  console.log(`Re-running ${tool.name} against ${probePath}`);

  let result: ToolResult;
  try {
    result = await tool.run(env.target, { path: probePath });
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    await submitProof({
      detected: false,
      severity: Severity.Info,
      title: `Validation re-run errored for ${env.vulnClass} at ${probePath}`,
      description: `Re-running ${tool.name} threw: ${msg}. Cannot confirm or refute the candidate; treating as unconfirmable.`,
      request: `re-run ${tool.name} at ${probePath}`,
      response: '',
      poc: '',
      reproductionSteps: '',
    });
    return;
  }

  // detected=true → severity from tool (reproduces vuln) → coordinator promotes parent to Validated
  // detected=false → severity Info (re-test clean) → coordinator promotes parent to Rejected
  const validationSeverity = result.detected ? result.severity : Severity.Info;
  const validationTitle = result.detected
    ? `Validation reproduced ${env.vulnClass} at ${probePath}`
    : `Validation re-test clean for ${env.vulnClass} at ${probePath}`;

  await submitProof({
    detected: result.detected,
    severity: validationSeverity,
    title: validationTitle,
    description: result.description,
    request: result.request,
    response: result.response,
    poc: result.poc,
    reproductionSteps: result.reproductionSteps,
  });
}

interface ProofSubmission {
  detected: boolean;
  severity: Severity;
  title: string;
  description: string;
  request: string;
  response: string;
  poc: string;
  reproductionSteps: string;
}

async function submitProof(p: ProofSubmission): Promise<void> {
  const probePath = env.scopePaths[0] ?? '/';
  const endpoint = `https://${env.target}${probePath}`;

  await agent.submitFinding({
    id: newFindingId(),
    kind: FindingKind.ValidationProof,
    vulnClass: env.vulnClass,
    parentFindingId: env.parentFindingId,
    severity: p.severity,
    title: p.title,
    endpoint,
    description: p.description,
    evidence: {
      request: Buffer.from(p.request),
      response: Buffer.from(p.response),
      poc: p.poc,
    },
    validationProof: {
      reproductionSteps: p.reproductionSteps,
      confirmed: p.detected,
      deterministic: true,
      validatorCageId: env.cageId,
    },
  });

  const decision = p.detected ? 'CONFIRMED' : 'REJECTED';
  console.log(`[${decision}] ${p.title}`);
}
