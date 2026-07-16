# The reasoner harness

`reasoner.py` is the reference brain `import --reasoning` writes into a generated
reasoning agent. It is one MCP tool, `respond(messages)`, that answers a request
by running an LLM tool-use loop over whatever tools it reaches through its `USES`
sub-agents. It ships as source, not a base image: the generated agent builds on a
stock Python image, so the file lands in the operator's directory to read and
edit, and `--reasoner <path>` swaps in a different one.

Nothing in it is mcpvessel-specific beyond the environment contract below. A
reasoner in any language is a drop-in replacement as long as it honors that
contract, which is the whole interface the runtime relies on.

## The environment contract

**Entrypoint.** Serve one MCP tool named after the agent's `MAIN` (`respond`
here) taking `messages` (a list of chat messages). `mcpvessel run <agent>
"<prompt>"` calls it with the prompt as the single user message.

**Serving.** Speak MCP over stdio when run as a root. When `VESSEL_SERVE_HTTP`
is set (a `host:port`), serve streamable HTTP at `/mcp` on that address instead;
that is how the gateway reaches the agent as a sub-agent. Boot cleanly with no
`USES` tools attached, so build-time introspection can list the tool without any
sub-agents running.

**Tools.** Each `USES` edge is injected as `VESSEL_USES_<ALIAS>_URL`, the
streamable-HTTP URL of that tool server behind the gateway. Connect each, list
its tools, and dispatch calls back to it. The `<ALIAS>` disambiguates when two
sub-agents expose a tool of the same name.

**LLM.** `VESSEL_LLM_URL` is an OpenAI-compatible endpoint. Send a placeholder
`model` and any api key; the LLM gateway holds the real provider key, rewrites
the `model` field to the operator's configured default, meters spend against the
run's budget, and forwards `tools`/`tool_choice` untouched, so native
function-calling works. A reasoner never sees or needs a provider key. The
endpoint speaks streaming too (`stream: true`), so a long completion does not
risk a read timeout and cost meters incrementally.

**Streaming (optional).** A REST caller can ask `mcpvessel serve` for
Server-Sent Events (`{"stream": true}` in the body, or `Accept:
text/event-stream`). serve sets an MCP progress token on the tool call and
turns the agent's progress notifications into SSE `delta` events, with a final
`done` carrying the whole result. To participate, emit `notifications/progress`
carrying each answer chunk in `message` during your tool call (this harness
does, via `ctx.report_progress`); FastMCP sends them only when the caller set a
token, so `run`, `call`, and non-streaming callers see no change. An agent that
emits none still works: the caller just gets one `done` event. So streaming is a
capability an agent opts into, not a mcpvessel-reasoner special case.

**Operator knobs.** Read configuration from non-`VESSEL_`-prefixed names; the
Vesselfile parser reserves that prefix for the runtime's own injected variables.
This harness reads `REASONER_MAX_TURNS`, `REASONER_MAX_RETRIES`, and
`REASONER_MAX_TOOL_CHARS`, and takes the operator's system prompt from
`REASONER_SYSTEM_PROMPT_FILE` (a file COPY'd into the image) or, as a fallback,
an inline `REASONER_SYSTEM_PROMPT`.

## The system prompt

The harness ships with a robust built-in prompt (tool-use discipline, no
fabricated results, honest uncertainty). The agent's author sets an additional
prompt at build time, which the harness appends to the built-in one, never
replacing it. Set it with `import --reasoning --prompt "..."` or, for a real
multi-line prompt, `--prompt-file ./prompt.md`. It lands in the generated agent
as `system_prompt.txt`, a plain file you can edit and rebuild. The prompt is the
agent's identity, so it is baked into the bundle and travels with `push`/`pull`,
not passed per run.

## One rule that is not optional

If any `USES` tool server the agent declares cannot be reached, refuse the run
rather than answer over a partial toolset and let the model claim it used a tool
it never received. Fail closed on the whole run, not just when every server is
down: a declared capability that dropped out is a failure, not something to paper
over. A toolless run is only legitimate when the agent declares no `USES` at all.
