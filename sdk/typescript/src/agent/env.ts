/**
 * Typed accessor for the AGENTCAGE_* environment variables that
 * cage-init exports to the agent process. The canonical contract
 * between orchestrator and agent — if you find yourself reading
 * process.env.AGENTCAGE_* directly, prefer this instead.
 *
 * Required fields (cageId, assessmentId, cageType, target) throw on
 * startup if missing — fail fast rather than silently doing nothing.
 * All other fields default to safe empty values.
 */

export type CageType = 'discovery' | 'exploitation' | 'validation';

export interface CageEnv {
  cageId: string;
  assessmentId: string;
  cageType: CageType;
  target: string;
  scopePaths: string[];
  scopePorts: string[];
  vulnClass: string;
  objective: string;
  llmEndpoint: string;
  llmApiKey: string;
  judgeAvailable: boolean;
  tokenBudget: number;
  proofThreshold: number;
  // Raw JSON string set by the operator via --credentials. The agent
  // author defines the schema; this SDK does not impose one. Parse to
  // whatever shape the agent expects (header/cookie/Bearer/etc.).
  targetCredentials: string;
}

export function readCageEnv(): CageEnv {
  return {
    cageId: required('AGENTCAGE_CAGE_ID'),
    assessmentId: required('AGENTCAGE_ASSESSMENT_ID'),
    cageType: parseCageType(process.env.AGENTCAGE_CAGE_TYPE),
    target: required('AGENTCAGE_SCOPE'),
    scopePaths: parseCsv(process.env.AGENTCAGE_SCOPE_PATHS),
    scopePorts: parseCsv(process.env.AGENTCAGE_SCOPE_PORTS),
    vulnClass: (process.env.AGENTCAGE_VULN_CLASS ?? '').trim(),
    objective: process.env.AGENTCAGE_OBJECTIVE ?? '',
    llmEndpoint: process.env.AGENTCAGE_LLM_ENDPOINT ?? '',
    llmApiKey: process.env.AGENTCAGE_LLM_API_KEY ?? '',
    judgeAvailable: process.env.AGENTCAGE_JUDGE_AVAILABLE === 'true',
    tokenBudget: parseNumber(process.env.AGENTCAGE_TOKEN_BUDGET, -1),
    proofThreshold: parseNumber(process.env.AGENTCAGE_PROOF_THRESHOLD, 0),
    targetCredentials: process.env.AGENTCAGE_TARGET_CREDENTIALS ?? '',
  };
}

function required(key: string): string {
  const v = (process.env[key] ?? '').trim();
  if (!v) {
    throw new Error(`${key} not set; cage-init did not populate this required env var`);
  }
  return v;
}

function parseCageType(raw: string | undefined): CageType {
  const v = (raw ?? '').toLowerCase().trim();
  if (v === 'discovery' || v === 'exploitation' || v === 'validation') {
    return v;
  }
  throw new Error(`AGENTCAGE_CAGE_TYPE invalid: got ${JSON.stringify(raw)}, want one of discovery|exploitation|validation`);
}

function parseCsv(raw: string | undefined): string[] {
  return (raw ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
}

function parseNumber(raw: string | undefined, fallback: number): number {
  if (!raw) return fallback;
  const n = Number(raw);
  return Number.isFinite(n) ? n : fallback;
}
