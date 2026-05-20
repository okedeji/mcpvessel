// Operator-supplied target credentials. The orchestrator injects
// AGENTCAGE_TARGET_CREDENTIALS as a JSON object — header mode for
// Bearer / API-key tokens, cookie mode for pre-authenticated session
// cookies. Multi-step login (form, OAuth, magic-link / OTP) is out
// of scope; pre-authenticate in a browser and supply the resulting
// header or cookie.

// Operator stores creds in Vault as a JSON string matching the shape in the provided agent, e.g.
// `agentcage vault put target <key> '{"type":"header","name":"Authorization","value":"Bearer ..."}'`.
export interface AuthContext {
  type: 'header' | 'cookie';
  name: string;
  value: string;
}

function parseAuth(): AuthContext | null {
  const raw = process.env.AGENTCAGE_TARGET_CREDENTIALS ?? '';
  if (!raw) return null;
  let parsed: any;
  try {
    parsed = JSON.parse(raw);
  } catch {
    console.log('AGENTCAGE_TARGET_CREDENTIALS is not valid JSON; running unauthenticated');
    return null;
  }
  if (parsed?.type !== 'header' && parsed?.type !== 'cookie') {
    console.log(`AGENTCAGE_TARGET_CREDENTIALS has unexpected type=${JSON.stringify(parsed?.type)} (want 'header' or 'cookie'); running unauthenticated`);
    return null;
  }
  if (typeof parsed.name !== 'string' || typeof parsed.value !== 'string') {
    console.log('AGENTCAGE_TARGET_CREDENTIALS missing name or value, or wrong type; running unauthenticated');
    return null;
  }
  if (!parsed.name || !parsed.value) {
    console.log('AGENTCAGE_TARGET_CREDENTIALS has empty name or value; running unauthenticated');
    return null;
  }
  return { type: parsed.type, name: parsed.name, value: parsed.value };
}

export const auth = parseAuth();

export function authHeaders(): Record<string, string> {
  if (!auth) return {};
  if (auth.type === 'header') return { [auth.name]: auth.value };
  return { Cookie: `${auth.name}=${auth.value}` };
}
