/**
 * JudgePayload is one classification request the proxy sends to a
 * judge service. Wire format: the endpoint receives POST with body
 * `{ "payloads": JudgePayload[] }` and must respond with
 * `{ "results": JudgeResult[] }` — one result per payload, in order.
 */
export interface JudgePayload {
  cageType: string;
  vulnClass: string;
  assessmentId: string;
  method: string;
  url: string;
  headers?: Record<string, string>;
  body: string;
  /** Per-cage instruction the orchestrator wrote when spawning this cage. */
  objective?: string;
  /** Per-request justification the agent supplied via X-Agentcage-Judge-Reason. */
  agentReason?: string;
}

export interface JudgeResult {
  safe: boolean;
  confidence: number;
  reason: string;
}
