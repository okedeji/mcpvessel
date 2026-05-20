import { fetch } from '@agentcage/sdk';
import { env } from './env';

export interface LLMMessage {
  role: 'system' | 'user' | 'assistant';
  content: string;
}

export interface ToolDefinition {
  name: string;
  description: string;
  parameters: object;
}

export interface ToolCall {
  name: string;
  arguments: Record<string, unknown>;
}

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (env.llmApiKey) headers['x-api-key'] = env.llmApiKey;
  return headers;
}

async function postLLM(body: object): Promise<any> {
  if (!env.llmEndpoint) {
    throw new Error('AGENTCAGE_LLM_ENDPOINT not set; agent cannot call LLM');
  }
  const resp = await fetch(env.llmEndpoint, {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify(body),
    timeoutMs: 60000,
  });
  if (!resp.ok) throw new Error(`LLM returned ${resp.status}`);
  return resp.json();
}

export async function askLLM(messages: LLMMessage[]): Promise<string> {
  const data = await postLLM({ messages });
  return data.choices?.[0]?.message?.content ?? '';
}

// callLLMWithTools issues an OpenAI-style chat-completions request with
// the `tools` parameter set. The LLM either picks a tool (returning a
// ToolCall with parsed arguments) or produces a free-text message
// (e.g. when no tool fits the request). tool_choice defaults to 'auto'
// so the model can decline to pick.
export async function callLLMWithTools(
  messages: LLMMessage[],
  tools: ToolDefinition[],
  opts: { toolChoice?: 'auto' | 'required' | 'none' } = {},
): Promise<{ toolCall: ToolCall | null; message: string }> {
  const data = await postLLM({
    messages,
    tools: tools.map((t) => ({
      type: 'function',
      function: { name: t.name, description: t.description, parameters: t.parameters },
    })),
    tool_choice: opts.toolChoice ?? 'auto',
  });
  const choice = data.choices?.[0];
  const rawCall = choice?.message?.tool_calls?.[0];
  if (rawCall?.function?.name) {
    let args: Record<string, unknown> = {};
    try {
      args = JSON.parse(rawCall.function.arguments ?? '{}');
    } catch {
      // LLM returned malformed JSON in arguments; treat as empty
    }
    return { toolCall: { name: rawCall.function.name, arguments: args }, message: '' };
  }
  return { toolCall: null, message: choice?.message?.content ?? '' };
}

// Strip markdown code fences that LLMs commonly wrap JSON in.
export function extractJSON(raw: string): any {
  let text = raw.trim();
  const fenceStart = text.indexOf('```');
  if (fenceStart >= 0) {
    const afterFence = text.indexOf('\n', fenceStart);
    const fenceEnd = text.lastIndexOf('```');
    if (afterFence >= 0 && fenceEnd > afterFence) {
      text = text.slice(afterFence + 1, fenceEnd).trim();
    }
  }
  return JSON.parse(text);
}
