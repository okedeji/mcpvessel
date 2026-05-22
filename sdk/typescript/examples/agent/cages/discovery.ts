// Discovery cage: agentic loop.
//
// Registers tools with the LLM. Each turn the LLM picks one tool, sees
// the result, picks the next. Capped at MAX_ITERATIONS to bound cost.
// Stops early when the LLM calls `done`.
//
// State injection: before every turn the loop hands the LLM a fresh
// "operator notes" message at the head of the message list — visited
// paths, technologies the recon scan named, findings already filed.
// The notes are rebuilt each iteration and never stored in history,
// so the LLM always sees current state without the conversation
// growing with stale duplicates.
//
// Pattern contrast with the exploitation cage: exploitation is a
// single-shot decision (pick one tool, run it, done). Discovery is
// an iterative loop because surface mapping is many small decisions
// — what to fetch next depends on what was just seen.

import { env } from '../lib/env';
import { callLLMWithTools, LLMMessage } from '../lib/llm';
import { DiscoveryTool } from '../tools/discovery/types';
import { fetchPath } from '../tools/discovery/fetch-path';
import { crawl } from '../tools/discovery/crawl';
import { probePaths } from '../tools/discovery/probe-paths';
import { dirFuzz } from '../tools/discovery/dir-fuzz';
import { reconScan } from '../tools/discovery/recon-scan';
import { submitFinding } from '../tools/discovery/submit-finding';
import { done } from '../tools/discovery/done';

const TOOLS: DiscoveryTool[] = [fetchPath, crawl, probePaths, dirFuzz, reconScan, submitFinding, done];
const TOOLS_BY_NAME = new Map(TOOLS.map((t) => [t.name, t]));

const MAX_ITERATIONS = 15;

const SYSTEM_PROMPT = `You are a penetration testing agent performing attack surface discovery.

Your goal: map the target's surface and submit Discovery findings for endpoints worth exploitation testing. The coordinator will read your findings to plan exploitation cages, so call out paths that look meaningfully exploitable (admin panels, auth endpoints, API routes, file uploads, exposed configs).

Each turn begins with an "Operator notes" user message summarizing what you have already done (visited paths, recon highlights, findings filed). Read it before deciding the next tool call. Do not re-fetch paths listed there. Do not brute-force surface you already have context for.

Strategy guidelines:
- Start with recon_scan on / to identify the tech stack and surface obvious exposures.
- Use fetch_path to look at one specific endpoint in detail when you need its body/headers.
- Use crawl when the target looks like a SPA or has heavy JavaScript (renders routes client-side).
- Use probe_paths to check candidate paths YOU generated from prior context — the tech stack you identified, references seen in HTML/JS bundles, conventions of the framework you detected (e.g. Next.js → /_next/data, Rails → /rails/info/routes, Spring → /actuator/*). This is your primary discovery tool once you have any context.
- Use dir_fuzz only as a last resort when you have NO context to generate candidates. If the operator notes already list a tech stack or any visited paths, you have context — use probe_paths instead.
- Submit findings as you discover them — do not hoard until the end. The coordinator works with findings as they arrive.
- Call done when you have a reasonable map of the surface. Better to stop slightly early than to burn iterations on diminishing returns.

You are bounded: maximum ${MAX_ITERATIONS} tool calls. Pace yourself.`;

interface SubmittedFinding {
  path: string;
  priority: string;
  reason: string;
}

interface DiscoveryState {
  visited: Set<string>;
  reconSummary: string;
  findings: SubmittedFinding[];
  iter: number;
}

function buildOperatorNotes(state: DiscoveryState): LLMMessage {
  const parts: string[] = [`Operator notes (iter ${state.iter}/${MAX_ITERATIONS}):`];
  if (state.reconSummary) {
    parts.push(`Recon highlights: ${state.reconSummary}`);
  }
  if (state.visited.size > 0) {
    const sorted = [...state.visited].sort();
    parts.push(`Visited paths (${state.visited.size}): ${sorted.join(', ')}`);
  } else {
    parts.push('Visited paths: none yet.');
  }
  if (state.findings.length > 0) {
    parts.push(`Findings filed (${state.findings.length}):`);
    for (const f of state.findings) {
      parts.push(`  - [${f.priority}] ${f.path} — ${f.reason.slice(0, 100)}`);
    }
  } else {
    parts.push('Findings filed: none yet.');
  }
  return { role: 'user', content: parts.join('\n') };
}

// extractReconSummary keeps the first ~250 chars of the recon_scan
// result. The full output stays in history; this short version
// surfaces the technology/exposure highlights at the head of every
// future prompt so the LLM doesn't have to scan back.
function extractReconSummary(toolResult: string): string {
  const trimmed = toolResult.trim();
  if (trimmed.length <= 250) return trimmed;
  return trimmed.slice(0, 250) + '…';
}

export async function runDiscovery(): Promise<void> {
  console.log(`\n── Discovery: target=${env.target} ──`);
  if (env.scopePaths.length > 0) {
    console.log(`Operator-supplied paths: ${env.scopePaths.join(', ')}`);
  }

  const initialUser: LLMMessage = { role: 'user', content: buildInitialPrompt() };
  const systemMsg: LLMMessage = { role: 'system', content: SYSTEM_PROMPT };

  // history holds the canonical conversation: assistant tool_call
  // messages and their matching tool responses. The operator notes
  // message is rebuilt every iter and never stored here, so it stays
  // current without bloating the conversation.
  const history: LLMMessage[] = [initialUser];

  const state: DiscoveryState = {
    visited: new Set<string>(),
    reconSummary: '',
    findings: [],
    iter: 0,
  };

  let stopped = false;

  for (let iter = 1; iter <= MAX_ITERATIONS && !stopped; iter++) {
    state.iter = iter;
    const messagesForLLM: LLMMessage[] = [systemMsg, buildOperatorNotes(state), ...history];
    const { toolCall, assistantMessage, message } = await callLLMWithTools(messagesForLLM, TOOLS, {
      toolChoice: 'required',
    });
    history.push(assistantMessage);

    if (!toolCall) {
      console.log(`Iteration ${iter}: LLM produced no tool call (msg: ${message.slice(0, 200)}). Stopping.`);
      break;
    }

    const tool = TOOLS_BY_NAME.get(toolCall.name);
    if (!tool) {
      const errMsg = `Unknown tool: ${toolCall.name}. Registered tools: ${TOOLS.map((t) => t.name).join(', ')}`;
      console.log(`Iteration ${iter}: ${errMsg}`);
      history.push({ role: 'tool', tool_call_id: toolCall.id, content: errMsg });
      continue;
    }

    console.log(`Iter ${iter}/${MAX_ITERATIONS}: ${toolCall.name}(${JSON.stringify(toolCall.arguments).slice(0, 120)})`);

    const result = await tool.run(toolCall.arguments);
    history.push({ role: 'tool', tool_call_id: toolCall.id, content: result });

    updateState(state, toolCall.name, toolCall.arguments, result);
    if (toolCall.name === 'done') stopped = true;
  }

  if (!stopped) {
    console.log(`Discovery hit iteration cap (${MAX_ITERATIONS}) without 'done'. Submitted ${state.findings.length} findings.`);
  } else {
    console.log(`\nDiscovery complete. Submitted ${state.findings.length} findings across ${state.visited.size} probes.`);
  }
}

function updateState(
  state: DiscoveryState,
  toolName: string,
  args: Record<string, unknown>,
  result: string,
): void {
  switch (toolName) {
    case 'fetch_path':
    case 'recon_scan': {
      const path = args.path as string | undefined;
      if (path) state.visited.add(path);
      if (toolName === 'recon_scan' && !state.reconSummary) {
        state.reconSummary = extractReconSummary(result);
      }
      break;
    }
    case 'crawl': {
      const seed = args.seed_path as string | undefined;
      if (seed) state.visited.add(seed);
      break;
    }
    case 'dir_fuzz': {
      const base = args.base_path as string | undefined;
      if (base) state.visited.add(base);
      break;
    }
    case 'probe_paths': {
      const paths = (args.paths ?? []) as string[];
      for (const p of paths) state.visited.add(p);
      break;
    }
    case 'submit_finding': {
      const path = (args.path as string) ?? '';
      const priority = (args.priority as string) ?? 'medium';
      const reason = (args.reason as string) ?? '';
      if (path) state.findings.push({ path, priority, reason });
      break;
    }
  }
}

function buildInitialPrompt(): string {
  const parts: string[] = [];
  parts.push(`Target: https://${env.target}`);
  if (env.objective) parts.push(`Objective: ${env.objective}`);
  if (env.scopePaths.length > 0) {
    parts.push(`Operator-supplied paths in scope (start here):\n${env.scopePaths.map((p) => `  ${p}`).join('\n')}`);
  } else {
    parts.push(`No operator-supplied paths. Start with recon_scan on / to characterize the target.`);
  }
  parts.push(`\nBegin discovery. Pick your first tool call.`);
  return parts.join('\n');
}
