import { fetch } from '@agentcage/sdk';
import { authHeaders } from './auth';

export interface HttpResponse {
  status: number;
  headers: Record<string, string>;
  body: string;
}

// fetchSafe wraps the SDK fetch (routes through the payload proxy)
// with auth-header injection, body truncation, and never-throw
// semantics. Returns null on any network error.
export async function fetchSafe(
  url: string,
  extraHeaders: Record<string, string> = {},
): Promise<HttpResponse | null> {
  try {
    const merged = { ...authHeaders(), ...extraHeaders };
    const resp = await fetch(url, { redirect: 'follow', headers: merged });
    const body = await resp.text();
    const headers: Record<string, string> = {};
    resp.headers.forEach((v, k) => {
      headers[k] = v;
    });
    return { status: resp.status, headers, body: body.slice(0, 8192) };
  } catch {
    return null;
  }
}
