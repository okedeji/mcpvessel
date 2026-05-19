import type { JudgePayload, JudgeResult } from '../types/judge';

export function validatePayloads(body: any): JudgePayload[] {
  if (!body || !Array.isArray(body.payloads)) {
    throw new Error('request must contain a "payloads" array');
  }
  for (let i = 0; i < body.payloads.length; i++) {
    const p = body.payloads[i];
    if (typeof p.cage_type !== 'string') throw new Error(`payloads[${i}].cage_type must be a string`);
    if (typeof p.vuln_class !== 'string') throw new Error(`payloads[${i}].vuln_class must be a string`);
    if (typeof p.method !== 'string') throw new Error(`payloads[${i}].method must be a string`);
    if (typeof p.url !== 'string') throw new Error(`payloads[${i}].url must be a string`);
  }
  return body.payloads.map((p: any) => ({
    cageType: p.cage_type,
    vulnClass: p.vuln_class,
    assessmentId: p.assessment_id ?? '',
    method: p.method,
    url: p.url,
    headers: (p.headers && typeof p.headers === 'object') ? p.headers : undefined,
    body: p.body ?? '',
    objective: typeof p.objective === 'string' ? p.objective : undefined,
    agentReason: typeof p.agent_reason === 'string' ? p.agent_reason : undefined,
  }));
}

export function validateResults(results: JudgeResult[], payloadCount: number): void {
  if (results.length !== payloadCount) {
    throw new Error(`expected ${payloadCount} results, got ${results.length}`);
  }
  for (let i = 0; i < results.length; i++) {
    const r = results[i];
    if (typeof r.safe !== 'boolean') throw new Error(`results[${i}].safe must be boolean`);
    if (typeof r.confidence !== 'number' || r.confidence < 0 || r.confidence > 1) {
      throw new Error(`results[${i}].confidence must be a number in [0, 1]`);
    }
    if (typeof r.reason !== 'string') throw new Error(`results[${i}].reason must be a string`);
  }
}
