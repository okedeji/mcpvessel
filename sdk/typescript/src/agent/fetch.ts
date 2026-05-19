// Node's fetch has no default timeout. Agents running inside cages talk
// to upstream targets through the payload-proxy, which intercepts TLS
// and forwards requests. On cold start (first request to a hostname),
// the proxy must generate a per-hostname cert from the cage CA — this
// can take a few seconds, especially when several requests run in
// parallel. A 30s default keeps cold-start scenarios reliable without
// hiding real outages.
const DEFAULT_TIMEOUT_MS = 30_000;

export interface FetchOptions extends RequestInit {
  /** Per-request timeout in milliseconds. Default: 30000. */
  timeoutMs?: number;
  /**
   * Ask the payload proxy to consult the LLM-as-a-judge before
   * forwarding this request. Set true on requests that could be
   * state-changing or destructive (POST/PUT/DELETE, SQL injection
   * probes that might modify data, etc.). Leave undefined for
   * read-only probes — judge calls cost LLM tokens.
   *
   * If a judge endpoint is configured, the proxy invokes it. If not,
   * the proxy holds the request and surfaces it as a payload-review
   * intervention so a human can decide. Check
   * process.env.AGENTCAGE_JUDGE_AVAILABLE to know which it'll be.
   */
  needsJudge?: boolean;
  /**
   * Short explanation of WHY this request needs review — what the agent
   * is trying to achieve with it. Forwarded to the judge LLM (and to
   * the human if the request is held). Examples: "enumerating UUIDs
   * 1-1000 to test IDOR", "demonstrating SQL injection via error-based
   * extraction on id parameter". Only sent when needsJudge is true.
   */
  judgeReason?: string;
}

/**
 * fetch is a drop-in replacement for Node's global fetch with a
 * platform-aware default timeout. Agents should prefer this over the
 * global fetch so they get sensible behavior under the cage proxy
 * without each agent reinventing timeout handling.
 */
export async function fetch(url: string | URL, init: FetchOptions = {}): Promise<Response> {
  const { timeoutMs = DEFAULT_TIMEOUT_MS, signal, needsJudge, judgeReason, headers, ...rest } = init;
  const timeoutSignal = AbortSignal.timeout(timeoutMs);
  // Compose with any caller-provided signal so external cancellation
  // still works alongside the timeout.
  const combined = signal ? AbortSignal.any([signal, timeoutSignal]) : timeoutSignal;

  const finalHeaders = new Headers(headers);
  if (needsJudge) {
    // The payload proxy strips X-Agentcage-* headers before forwarding
    // to the target, so the target never sees agentcage internals.
    finalHeaders.set('X-Agentcage-Judge', 'required');
    if (judgeReason) {
      finalHeaders.set('X-Agentcage-Judge-Reason', judgeReason);
    }
  }

  return globalThis.fetch(url, { ...rest, signal: combined, headers: finalHeaders });
}
