# replay

Record an agent run's full payloads so you can keep it, share it when you file a bug, or pick it apart yourself. `replay` runs an agent exactly like `run`, but with full-payload capture on: every LLM call's request and response, plus every sub-agent tool call, is written to a `.replay` file under `~/.mcpvessel/replays`. Capture is heavy, so a normal `run` never does it. `replay record` is how you turn it on.

```
mcpvessel replay record BUNDLE [PROMPT]
```

`replay` on its own is a grouping command with one subcommand, `record`. Run it with no subcommand and it prints help.

## record

```
mcpvessel replay record BUNDLE [PROMPT]
```

`record` boots the agent through the daemon, routes your prompt to its `MAIN` tool the same way `run` does, and returns the tool's result on stdout. Alongside that it captures the run's external interactions and writes them to `~/.mcpvessel/replays/<run-id>.replay`. When the run finishes it prints, on stderr, how many events it recorded and the file it wrote.

`BUNDLE` resolves the same way it does for `inspect`: a `.agent` file, a content hash an untagged build printed, a `@org/name:version` reference (store first, pulled from the registry only if the store lacks it), or a reverse-DNS MCP Registry name resolved to a reference. Unlike `run`, `record` does not accept a source directory. It runs a built bundle, not one it builds for you, so build the directory first (`mcpvessel build`) and record the result.

`PROMPT` is optional. When given, it becomes a single user turn (`{"role":"user","content":PROMPT}`) in a `messages` array and is passed to `MAIN`, the `{role, content}` shape both OpenAI and Anthropic accept. Omit it (or pass an empty string) and `MAIN` is called with no arguments, for an agent that needs no prompt. `record` takes at most two positional arguments and rejects a third.

The bundle must have a `MAIN`. `record` reads the sealed manifest and, if `MAIN` is empty, refuses with an error saying it records an agent's `MAIN`, not a tool collection. A tool collection has no single entry to drive, so there is nothing to record. To exercise one of its tools, use `call`.

`record` takes the same input flags as `run` (`--budget`, `--secret`, `--secret-file`, `--env`, `--env-file`, `--egress`; the table below), so a run that needs a secret or a host allowance records the same way it runs. What it does not take is `run`'s resource and build knobs: no `--memory`, `--cpus`, `--pids`, `--no-cache`, or `--save`. Those shape how a bundle is built or capped, and `record` runs an already-built bundle at default caps.

`record` needs the daemon. If it cannot reach it, the error tells you to run `mcpvessel init` to start it.

## What a .replay holds

The artifact is a single JSON document, schema version `0.1`. The daemon assembles it at the run's finish, after the tool call returns but before it tears the run's gateways down (the payloads live in the gateway logs and must be read while the containers are still up).

Top level:

- **`version`**: the artifact schema version (`0.1`).
- **`agent_ref`**: the bundle as you named it on the command line.
- **`agent_manifest_hash`**: the content hash of the bundle's files, so you know exactly which build produced the run.
- **`run_id`**: the daemon's id for the run, the same string as the `.replay` filename. It is the agent's name, a short digest of its files, and a unique suffix, so every recording is its own file and repeated runs never collide.
- **`input`**: the `tools/call` that started the run, as `{tool, args}`. `tool` is the resolved `MAIN`; `args` is the `messages` array built from your prompt (absent when you gave none).
- **`events`**: the ordered interactions (below).
- **`egress`**: present only with `--egress-inspect`. Each entry is one inspected outbound request to an approved host: `{host, agent, method, url, req_header, req_body, status, resp_header, resp_body}`, headers and bodies verbatim. A connection that could not be inspected (cert pinning, HTTP/2) carries a `note` instead. A body past 64KB is captured to the cap and marked `truncated`.
- **`started_at`** / **`ended_at`**: the run's wall-clock bounds.
- **`result`**: the run's outcome, as `{output, status, error}`. `status` is `succeeded` with the tool's `output`, or `failed` with the `error` string.

Each entry in `events` is one external interaction, numbered by `seq` in start-time order across both LLM and sub-agent calls:

- **`type`**: `llm.complete` for a non-streamed LLM call, `llm.stream` for a streamed one, or `subagent.<edge>.<tool>` for a call this agent made into a `USES` sub-agent.
- **`request`** / **`response`**: the captured bodies. When the body was JSON it is embedded raw; when it was not (a streamed SSE response) it is embedded as a JSON string, so the artifact stays valid JSON either way. For a sub-agent event, `request` is the tool's arguments.
- **`tokens_in`** / **`tokens_out`** / **`cost_micro_usd`**: the metered prompt tokens, completion tokens, and cost of an LLM call, in millionths of a USD. Sub-agent events do not carry these.
- **`t_unix_nano`**: the call's start time in Unix nanoseconds.

The request bodies are the agent-facing bodies the LLM gateway sees, captured before the proxy attaches your provider key. A recording never contains a key.

## How the recording is made and saved

Two writes happen, and the second is the one you keep.

1. The daemon runs the agent with capture on. When the tool call returns, it reads the LLM call records off the LLM gateway's log and the sub-agent records off the MCP gateway's log, merges and orders them, and writes the `.replay` itself. This daemon-side write is best effort: if it fails, the daemon warns and the run still succeeds.
2. The CLI then fetches the same artifact back from the daemon over its socket (`GET /runs/<run-id>/replay`) and writes a host copy to `~/.mcpvessel/replays/<run-id>.replay` with `0600` permissions, decodes it to count the events, and prints `Recorded N event(s) to <path>`.

On one machine both writes land at the same path, so you end with one file. The location honors `VESSEL_HOME`; with it unset the root is `~/.mcpvessel`.

If the run finishes but the CLI cannot fetch the artifact back, it says so (`the run finished but its replay could not be fetched`) rather than pretend it saved one.

## Flags

`record` takes the same input flags `run` does, so a recorded run is exactly the run it would have been:

| Flag | Meaning |
| --- | --- |
| `--budget USD` | Cap the run's LLM spend, e.g. `5.00`. Overrides the agent's advisory `BUDGET`. |
| `--secret NAME` | Supply a secret the agent declares, or `agent:NAME` to grant one agent of several. Repeatable. |
| `--secret-file PATH` | Read secret values (`[agent:]NAME=VALUE` per line) from a permissions-restricted file. |
| `--env KEY=VALUE` | Supply an env value, or `KEY` to pass it through from your environment. Repeatable. |
| `--env-file PATH` | Read env values (`KEY=VALUE` per line) from a file. |
| `--egress HOSTS` | Allow the agent hosts for this run: `host,host`, or `agent:host,host` to scope one. Repeatable. |
| `--egress-inspect` | Decrypt the run's outbound HTTPS to an approved host and fold the full captures (headers and bodies, both ways) into the recording under a top-level `egress` array. Opt-in. |

The LLM and sub-agent captures never contain a secret or the provider key: request bodies are captured before the gateway attaches the key, and secret values ride env injection, not the recorded payloads. The `--egress-inspect` capture is the exception: it records what a server actually sent an approved host, verbatim, so a secret the server shipped is in the `egress` array as sent. Treat a recording made with `--egress-inspect` as sensitive.

## Examples

```sh
# Record a run against a tagged agent, with a prompt.
mcpvessel replay record @okedeji/researcher:0.1 "summarize Q3 earnings"

# Record a run that needs no prompt.
mcpvessel replay record @me/hello:0.1

# Record from a local .agent file.
mcpvessel replay record ./researcher.agent "who paged us last night"

# Record by content hash, then read the artifact back.
mcpvessel replay record sha256:1f3c... "test prompt"
cat ~/.mcpvessel/replays/researcher-1f3c-*.replay | jq '.events[].type'
```

## Notes

- Capture is expensive, which is why only `record` turns it on and a plain `run` never does. Record when you need the payloads, run otherwise.
- The `.replay` is an artifact to keep, share, or analyze. mcpvessel does not re-drive it: there is no command that feeds a recording back through the system to reproduce the run. What you do with the file is up to you.
- A tool collection cannot be recorded. `record` drives `MAIN`, and a tool collection has none. Use `call` to exercise its tools.
- A source directory is not a valid `BUNDLE` here. `record` runs an already-built bundle. Build the directory first.
- No provider key ever reaches the file: requests are captured before the gateway attaches the key. The artifact can still contain whatever your prompt, the model's responses, and your tools' arguments and results held, so treat it as sensitive before sharing.
- A failed run still records. `result.status` is `failed` with the error, and every event up to the failure is in the file.
- `record` accepts `--egress-inspect`, which decrypts the run's outbound HTTPS (see [egress](egress.md#seeing-what-a-server-sends---egress-inspect)). Each request surfaces live in `mcpvessel events` and `mcpvessel logs` as a metadata summary, and the full captures, request and response headers and bodies both ways, are written into the recording under a top-level `egress` array. They are verbatim and unredacted, the same posture as the LLM payloads above: a secret a server shipped to an approved host is in the file as sent, so treat the artifact as sensitive.

## See also

- [run](run.md): the same boot, `MAIN` routing, and input flags without capture, plus the resource and build flags (`--memory`, `--cpus`, `--pids`, `--no-cache`, `--save`) `record` does not take.
- [inspect](inspect.md): resolves a `BUNDLE` the same way, to see what an agent is before you record it.
- [trace](trace.md), [spend](spend.md): the other ways to watch what a run does.
- [call](call.md): drive a single tool of a tool collection, which `record` cannot record.
- [Commands](../README.md#commands): the full command list.
