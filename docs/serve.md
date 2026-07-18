# serve

Open one HTTP front door for one or more built agents so an external MCP client (Cursor, Claude) or a plain `curl` can call their tools. `serve` merges every agent's public tools onto a single URL, gives each agent its own endpoint too, and prints the exact egress and secrets each will get before any traffic flows. It talks to the daemon, which owns the runs and keeps serving after `serve` returns.

```
mcpvessel serve BUNDLE... [flags]
```

`serve` opens handlers only. The daemon boots the caged instances, lazily and per client, and holds them until you `mcpvessel stop` the runs or it shuts down. `serve` itself returns as soon as the front door is listening.

## What a BUNDLE can be

Each BUNDLE names an agent to expose. Four forms are accepted, and you can mix them in one command.

- **A reference** (`@me/researcher:0.1`, `ghcr.io/org/name:tag`). Resolved store-first, then pulled if absent. The reference's repository becomes the root agent's address.
- **A content hash** from an untagged build. Served straight from the store.
- **A path to a `.agent` file.** Served as is.
- **A source directory with a Vesselfile.** A directory already built or imported serves its stored bundle without a rebuild (see [Source directories](#source-directories)).

A directory argument with no Vesselfile is passed through to the daemon's locate, which returns a clearer error than `serve` would.

## Source directories

When a BUNDLE is a directory holding a Vesselfile, `serve` resolves it by content hash rather than by name. It hashes the source, and:

- If a bundle with that hash is already in your store (an earlier `import` or `build` introspected it), that stored bundle is served as is, no rebuild.
- Otherwise the directory is built into the store first, then served.

Either way the directory's base name becomes the agent's address, because a content-hash prefix would make a poor URL segment. So `./server-github` serves under `/agents/server-github/mcp`.

## The front door

`serve` mounts several kinds of endpoint on the one listen address.

**The merged endpoint at `/mcp`.** One MCP server advertising every exposed agent's public tools at once, each renamed `<agent>_<tool>`. This is the URL you give an MCP client: it configures one endpoint no matter how many bundles sit behind it. Every tool name is prefixed, never only the colliding ones, so adding a bundle to a serve can never rename a tool out from under a client that already points at it. If two agents' addresses sanitize to the same prefix and a tool would collide even after prefixing, that is a hard error telling you to hide one agent with `--no-expose`, not a silent drop. The merged endpoint appears only when at least one public tool exists.

**A per-agent endpoint at `/agents/<address>/mcp`.** Each exposed agent also gets its own MCP-over-HTTP endpoint, where its tools keep their bare names (no `<agent>_` prefix).

**Plain HTTP on the same port.** A second door into the same caged instances and the same dispatch, not a bypass. Three POST routes:

- `POST /agents/<address>/tools/<tool>` invokes one tool. The JSON request body is the tool's own arguments; an empty body means no arguments. A tool outside the agent's public set is a 404.
- `POST /agents/<address>` prompts the agent's main tool. The body is `{"prompt": "..."}`, or a raw `{"messages": [...]}` array; a bare prompt is wrapped as one user message the way `run` does. This route exists only for an agent that has a `MAIN`. A tool collection with no `MAIN` has no prompt route.
- `POST /tools/<name>` invokes a tool on the merged endpoint by its prefixed `<agent>_<tool>` name.

A successful plain-HTTP call returns `{"result": "..."}`; an error returns `{"error": "..."}` with an HTTP status (400 for a failed call, 404 for an unknown tool).

## Streaming answers over SSE

Any plain-HTTP call can ask for Server-Sent Events instead of one buffered JSON object. Three ways to ask, all out of band so the request never lands in the tool's arguments:

- `Accept: text/event-stream` (what an `EventSource` sends).
- `?stream=1`, `?stream=true`, or `?stream=yes` on the URL.
- `"stream": true` in a `POST /agents/<address>` prompt body (the prompt envelope is ours, so this field is clean; it does not work on the raw tool-call route, whose body is the arguments).

The stream emits `delta` events carrying answer chunks as the agent generates them, a final `done` event carrying the whole result, and an `error` event on failure. An agent that reports no progress, or a target with no streaming path, still yields a valid stream: one `done` with the full answer. An idle stream sends a keepalive comment every 15 seconds so a proxy does not time out during a long tool-call phase that emits no deltas. Any agent that emits MCP progress notifications streams here for free; the event vocabulary is the reasoner contract's. If the underlying writer cannot flush, `serve` falls back to a single JSON object rather than buffer an SSE body the caller only sees at the end.

## What gets exposed

A named agent is always exposed. So is any `USES PUBLIC` sub-agent in its tree, and exposure propagates down a chain only while every edge stays public: a private sub-agent, and everything below it, is unreachable. Overrides:

- **`--expose REF`** force-exposes an agent that is not public, matched by registry and repository (a version-less `@org/name` catches every pin).
- **`--no-expose REF`** hides an agent even if it is `USES PUBLIC`, same matching. `--no-expose` is applied last and wins over everything, including `--expose`. The served root is never hidden.

Each exposed agent advertises only its public tools (its `MAIN` plus its `EXPOSE`'d tools), read straight from the bundle's static catalog. No instance boots just to list tools; private tools are never registered, so registration is the access gate.

## Addresses

An agent's URL segment is derived and sanitized to URL-safe characters:

- The **root** takes its reference's repository. `@me/researcher:0.1` serves as `me-researcher`, `@me/x` as `me-x` (the slash and any other unsafe character become a hyphen).
- A **sub-agent** takes its own repository, sanitized the same way.
- A **source directory** takes its base name.

Two exposed agents resolving to one address is an error, not last-writer-wins over the routing table. Hide one with `--no-expose`.

## Egress

A caged agent reaches no host unless allowed. Three sources of allowance, and `serve` prints the effective union per agent before opening the door:

- **A bundle's baked `EGRESS`** applies with no flag at all. A pulled bundle carries its author's declared hosts; the boot-time Egress report is where you see them.
- **`--egress host,host`** allows hosts for every served agent, for this run only, never touching the bundle. **`--egress agent:host,host`** scopes hosts to one agent by its address, so a batch can give each its own allowance (hosts never contain a colon, so the colon unambiguously separates the scope).
- **`--save`** with `--egress` writes the hosts into the agent's Vesselfile (`EGRESS allow:` line, unioned with what is there) and rebuilds, so they travel with the bundle from then on. It needs a source directory to edit; a built or pulled bundle has no local source, so `--save` on one is a clear error pointing you to `--egress` alone or a re-import. With `--save` the hosts are baked, so the per-run override then adds nothing.

The boot-time **Egress:** report lists each agent's effective allowlist, marking which hosts came from the bundle and which from `--egress`. An agent allowed nothing reads `none (no network)`. A blocked host is named in the tool error the caller gets back and in `mcpvessel logs`.

## Secrets and env

An agent often needs a key or a config value to boot. `serve` collects them into pools and injects each into any served instance that declares its name.

- **`--secret NAME`** grants a secret, resolved from your environment first, then the mcpvessel secret store, never the command line. **`--secret agent:NAME`** pins it to one agent; a bare `NAME` broadcasts to every agent declaring it. A name that is in neither your environment nor the store is a hard error telling you to `mcpvessel secrets set NAME`.
- **`--secret-file PATH`** reads many at once, one `[agent:]NAME=VALUE` per line, from a permissions-restricted file. Blank lines and `#` comments are skipped.
- **`--env KEY=VALUE`** supplies a plain config value; **`--env KEY`** (no value) passes it through from your environment. **`--env-file PATH`** reads `KEY=VALUE` lines from a file.

The boot-time **Secrets:** report lists, per agent, each declared secret and whether the pool will satisfy it: `(granted)`, `(optional, not granted)`, or `(missing; pass --secret NAME)`. Names only, never values. A required secret left missing fails that agent's boot; an optional one does not.

## Prebuilt images

Before the front door opens, `serve` builds every image the serve's instances will need: each exposed agent's full `USES` tree, plus the shared gateway image when a tree needs one. This is synchronous on purpose. A background build would only narrow the race with a client's first call, and a build failure (an npm or pip install that fails, longer than an MCP client's call timeout) belongs in your terminal, not inside an MCP error in Cursor. Everything is content-addressed, so an already-built bundle costs only an existence check.

## The daemon

`serve` needs a running daemon. If it cannot reach one, it tells you to run `mcpvessel init` to start it. Each per-client instance the front door boots is a real run: it shows in `mcpvessel logs` and on the run feed, and it is torn down by the instance manager when idle, not by any one request. Distinct MCP clients (keyed by session id) get their own instances so they run concurrently; all plain-HTTP callers to one agent share a single instance. The front door itself is a pool, not a run, and stays off the feed. `serve` returns once the door is listening; stop the runs with `mcpvessel stop` or shut the daemon down to close it.

## Elicitation

Over the MCP endpoints, an agent that asks a mid-call question routes it to the calling client via MCP elicitation. A client without the elicitation capability makes the asking call fail closed rather than hang. Over plain HTTP there is no answer channel: a `curl` caller cannot answer, so an agent that asks fails closed there too.

## Flags

| Flag | Meaning |
| --- | --- |
| `--listen ADDR` | Address to bind the front door to, e.g. `:7000` or `127.0.0.1:7000`. Required. A bind failure (port in use) is an error. |
| `--expose REF` | Also expose this agent even if it is not public, matched by registry and repository. Repeatable. |
| `--no-expose REF` | Hide this agent even if `USES PUBLIC`, same matching. Applied last, wins over `--expose`. The root is never hidden. Repeatable. |
| `--egress HOSTS` | Allow a served agent hosts for this run: `host,host`, or `agent:host,host` to scope one of several by its address. Repeatable. Default is no network. |
| `--save` | With `--egress`, write the hosts into the agent's Vesselfile and rebuild instead of allowing them for this run only. Source directories only; an error on a built or pulled bundle. |
| `--secret NAME` | Supply a secret a served agent needs, or `agent:NAME` to grant one of several. Resolved from your environment or the secret store, never the command line. Repeatable. |
| `--secret-file PATH` | Read secret values (`[agent:]NAME=VALUE` per line) from a permissions-restricted file. |
| `--env KEY=VALUE` | Supply an env value a served agent needs, or `KEY` to pass it through from your environment. Repeatable. |
| `--env-file PATH` | Read env values (`KEY=VALUE` per line) from a file. |

## Examples

```sh
# Serve one agent on a public URL.
mcpvessel serve --listen :7000 @me/researcher:0.1

# Serve two source directories on one loopback URL; each keeps its dir name as its address.
mcpvessel serve --listen 127.0.0.1:7000 ./server-github ./mcp-server-time

# Serve two agents, granting hosts to only one of them, plus a secret it needs.
mcpvessel serve --listen 127.0.0.1:7000 \
  --egress me-github:api.github.com \
  --secret me-github:GITHUB_PERSONAL_ACCESS_TOKEN \
  @me/github:0.1 @me/researcher:0.1

# Hide a private credential agent from the front door even if a USES PUBLIC edge would reach it.
mcpvessel serve --listen 127.0.0.1:7000 --no-expose @me/creddb @me/researcher:0.1

# Prompt a served agent's main tool with curl, streaming the answer over SSE.
curl -N -H 'Accept: text/event-stream' \
  -d '{"prompt":"summarize the latest incident"}' \
  http://127.0.0.1:7000/agents/me-oncall
```

## Notes

- `--listen` is required; there is no default port.
- The merged `/mcp` endpoint is the one URL to hand an MCP client. The per-agent `/agents/<address>/mcp` endpoints exist for a client that wants one agent's bare tool names.
- Adding a bundle to an existing serve never renames a tool on `/mcp`, because every name is always prefixed. But two agents whose addresses sanitize identically can still collide; `serve` errors rather than drop one.
- `--save` mutates and rebuilds your source directory. Without it, `--egress` is purely for the current serve and never touches the bundle.
- The boot-time Egress and Secrets reports are the last chance to see what the cage will get before any call runs. A pulled bundle's baked egress applies with no flag, so it shows there.
- The plain-HTTP prompt route (`POST /agents/<address>`) exists only for an agent with a `MAIN`. A tool collection has none; call its tools by name at `POST /agents/<address>/tools/<tool>`.
- `serve` is deny-default. A served server reaching a new host has the connection held, surfaced on the `events` feed and `mcpvessel egress ls`; approve it with [egress](egress.md) allow and it is remembered for next time.

## See also

- [import](import.md), [build](build.md): produce the bundle `serve` exposes.
- [run](run.md), [call](call.md): drive an agent from the CLI instead of over HTTP.
- [egress](egress.md): approve the hosts a served agent is held on.
- [VESSELFILE.md](VESSELFILE.md): `EGRESS`, `EXPOSE`, `USES PUBLIC`, `MAIN`, and `SECRETS`, the directives that decide what `serve` exposes and injects.
- [Cage it](../README.md#cage-it) and [Give it a brain](../README.md#give-it-a-brain): end-to-end walkthroughs that serve a caged server and a reasoning agent.
