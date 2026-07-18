# run

Run an agent once and print its answer. `run` resolves a bundle, boots a fresh cage, routes your prompt to the tool the Vesselfile declared as `MAIN`, prints the result, and tears the cage down. One boot, one call, one teardown. Nothing persists: the platform keeps no conversation state, so a second `run` is a clean container that remembers nothing.

```
mcpvessel run BUNDLE [PROMPT] [flags]
```

`run` is for an agent with a `MAIN` (a reasoning agent you give a goal). A bundle with no `MAIN` is a tool collection, and `run` refuses it, pointing you at [call](call.md) to invoke a named tool instead. `run` needs the daemon; `mcpvessel init` starts it (and provisions the Linux VM on macOS the first time).

## What a BUNDLE can be

The first argument resolves through the same path `serve` uses, so the four forms behave identically here.

**A source directory.** A directory holding a `Vesselfile` is built into your store before the run, with build progress on your terminal, and the resulting content hash is what the daemon boots. An unchanged directory is a cheap store hit, not a rebuild, so `run ./dir` iterates without a manual `build` step. A directory with no `Vesselfile` is a clear error, not a silent skip: point at an agent source directory, a reference, a content hash, or a `.agent` file.

**A reference** like `@me/oncall:0.1`, put in the store by `build -t` or `import -t`. A reference resolves store-first and is pulled from the registry only when your store does not already hold it.

**A content hash**, the bare `sha256:...` an untagged build printed. It addresses a bundle already in your store by its content alone.

**A path to a `.agent` file**, a bundle on disk that has not been imported into the store.

The friendly name used to scope `--egress` and `--secret` comes from the target: the base directory name for a source directory, the bundle's short name otherwise.

## The prompt

The optional second argument is the prompt. `run` wraps it as a single user turn, the `{role, content}` messages array both OpenAI and Anthropic accept:

```json
{"messages": [{"role": "user", "content": "<PROMPT>"}]}
```

That map is the argument object for the `MAIN` tool. Omit the prompt and `run` sends an empty argument object, letting a `MAIN` that needs no input still run. Because the platform stores no conversation state, prior turns are yours to carry: `run` never sends more than the one turn you give it.

## Routing to MAIN

`run` reads the resolved bundle's sealed manifest and uses its `MAIN` as the tool to call. `MAIN` is the tool the Vesselfile named as the agent's reasoning entry point. If the manifest has no `MAIN`, the bundle is a tool collection with nothing to "run," and `run` stops before booting anything:

```
bundle @me/fetch:0.1 has no MAIN; it is a tool collection. Use
'mcpvessel call @me/fetch:0.1 TOOL --arg KEY=VALUE' to call one of its tools directly
```

## The boot report

Before the cage boots, `run` prints two lines to stderr so you see exactly what the run will grant before any traffic or credential enters the cage.

**egress** is the effective outbound allowlist and where each host came from: hosts the bundle's author baked in (`from bundle`), hosts you added with `--egress` (`from --egress`), or `none` when both are empty. An operator host already baked is not repeated. This is the starting allow-set, not a hard ceiling: a run is deny-default, so a host outside it is held at run time and you approve it with [egress](egress.md) allow rather than the call failing. A bundle that declares `EGRESS deny-default` is the exception, hard-isolated with no network at all.

**secrets** is each secret the agent declares against the pool it will draw from: `granted`, `optional, not granted` for one marked optional in the Vesselfile, or `missing; pass --secret NAME` for a required one you did not supply (fatal at boot). Names only, never values.

Both lines go to stderr. The tool result is the only thing on stdout, so `mcpvessel run ... > answer.txt` captures the answer alone.

## Inputs the agent needs

An agent often needs an API key or a config value to work. You supply those per run, and they are injected into the cage at boot, scoped to what the agent declares. An undeclared value is never injected.

- **`--secret NAME`** grants a secret. Its value is resolved from your environment first, then the mcpvessel secret store, never from the command line, so it stays out of your shell history and the process table. A name found in neither place fails closed with a message telling you to store it first (`mcpvessel secrets set NAME`, which reads the value from stdin) or export it.
- **`--secret agent:NAME`** grants a secret to just one agent of several (matched by short name: a run name or a `USES` alias), the way `--egress agent:host` scopes hosts. A bare `NAME` grants every agent that declares it. Either way the value always resolves by the bare `NAME`; the scope only decides who receives it.
- **`--env KEY=VALUE`** supplies a plain config value. **`--env KEY`** (no value) passes it through from your environment.
- **`--secret-file`** and **`--env-file`** read many at once, one entry per line, blank lines and `#` comments skipped. A secret-file line is `[agent:]NAME=VALUE`; an env-file line is `KEY=VALUE`. A line that is not in that shape is an error.

A declared secret or a value-less required `ENV` input with nothing supplied fails the boot, unless the Vesselfile marked it optional, in which case it is simply left absent.

## Egress for this run

An agent is deny-default: it reaches no host the bundle did not bake in. `--egress` grants more, for this run only.

- **`--egress host,host`** allows those hosts for the run.
- **`--egress agent:host,host`** scopes hosts to one agent by name, so a composed run can give each sub-agent its own allowance.

Hosts granted this way are added on top of the bundle's baked `EGRESS`, they do not replace it, and they last only for the single run. To make the grant permanent, use `--save`.

## --save: bake the hosts in

`--save` turns `--egress` from a one-run grant into a permanent one. Instead of allowing the hosts for this run, it unions them into the target's `Vesselfile` `EGRESS allow:` line (creating the directive if absent, keeping existing hosts, stable order so a re-save is a no-op diff), rebuilds the bundle, and prints what it saved. Because the hosts are now baked, they are not also applied as a per-run override.

`--save` needs editable source. A pulled or otherwise built bundle has no local Vesselfile to write, so `--save` against one is a clear error telling you to allow the hosts for the run with `--egress` alone, or re-import the server with `--egress` and rebuild. The rebuild reintrospects the server, so it needs the same `--secret` and `--env` inputs the server needs to boot; pass them alongside `--save`.

## Budget

**`--budget`** caps the run's LLM spend in USD, for example `--budget 5.00`. It overrides the agent's advisory `BUDGET` directive (advisory because the author's number is a hint; the operator's `--budget` is what the run enforces). The amount is parsed to micro-USD, so at most six decimal places are allowed; a finer number is rejected. Zero is rejected too: omit the flag to leave the run unbounded, do not pass `--budget 0`. A run that overspends and fails still reports what it burned.

## Resource caps

Three flags cap the cage's resources for this run, each overriding the configured default. A malformed or non-positive value fails closed, never treated as unlimited.

- **`--memory`** is a size like `512m` or `2g`.
- **`--cpus`** is a number of cores like `2` or `0.5`.
- **`--pids`** is a positive process-count limit. Passing `--pids 0` or negative is an error.

## Rebuild control

**`--no-cache`** rebuilds every image from scratch on first use, ignoring cached layers and already-built images. Use it when a base image moved under a floating tag and you want the current bytes rather than what the cache holds.

## Flags

| Flag | Meaning |
| --- | --- |
| `--budget USD` | Cap the run's LLM spend, e.g. `5.00`. Overrides the agent's advisory `BUDGET`. Must be positive; at most six decimal places. Omit for unbounded. |
| `--secret NAME` | Grant a secret the agent needs, resolved from your environment or the secret store (never the command line). `agent:NAME` scopes it to one agent of several. Repeatable. |
| `--secret-file PATH` | Read secret values (`[agent:]NAME=VALUE` per line) from a permissions-restricted file. |
| `--env KEY=VALUE` | Supply an env value, or `KEY` to pass it through from your environment. Repeatable. |
| `--env-file PATH` | Read env values (`KEY=VALUE` per line) from a file. |
| `--egress HOSTS` | Allow the agent hosts for this run: `host,host`, or `agent:host,host` to scope one. Added on top of the bundle's baked egress. Repeatable. With none, the run is deny-default and holds a new host for approval. |
| `--save` | With `--egress`, write the hosts into the agent's Vesselfile and rebuild instead of allowing them for this run only. Source directories only. |
| `--memory SIZE` | Per-cage memory cap for this run, e.g. `2g`. Overrides the configured default. |
| `--cpus N` | Per-cage CPU cap for this run, e.g. `2` or `0.5`. |
| `--pids N` | Per-cage pids cap for this run. Must be positive. |
| `--no-cache` | Rebuild every image from scratch, ignoring cached and already-built images. |

## Examples

```sh
# Run a stored reasoning agent with a goal.
mcpvessel run @me/oncall:0.1 "why did checkout error rates spike at 14:00 UTC?" \
  --secret SENTRY_ACCESS_TOKEN --secret BRAVE_API_KEY

# Iterate on a source directory: built on change, booted, prompted.
mcpvessel run ./oncall "summarize the last hour of alerts"

# Run a bundle straight from a .agent file, capturing just the answer.
mcpvessel run researcher.agent "summarize Q3 earnings" > answer.txt

# Cap spend and give one sub-agent its own host allowance for this run.
mcpvessel run @me/oncall:0.1 "triage the paging alert" --budget 2.50 \
  --egress me-sentry:sentry.io --secret SENTRY_ACCESS_TOKEN

# Discover a host during a run, then bake it into the source and rebuild.
mcpvessel run ./oncall "fetch the status page" --egress status.example.com --save
```

## Notes

- The result is on stdout; the egress line, the secrets line, and the agent's own logs are on stderr. Redirect stdout to capture the answer alone.
- A run is stateless. There is no memory across runs and no follow-up turn; carry prior context yourself in the prompt you send.
- A first-use image build can take minutes and does not count against any timeout: the deadline the daemon applies covers the tool call, not the boot. `run` itself sets no timeout, so the call runs until it finishes or you cancel.
- Cancelling the command (Ctrl-C) cancels the boot and the call on the daemon; the run does not keep going detached.
- If the daemon is not running, `run` fails with a message pointing you at `mcpvessel init`. On macOS the first `init` also provisions the Linux VM the cages run in.
- `--secret` and `--env` inject only inputs the agent declares. Granting a name the agent does not declare is harmless: it is simply not injected, so a shared secret pool never leaks into a sub-agent that did not ask for it.
- `--save` rebuilds, and the rebuild reintrospects the server, so pass the same secrets and env the server needs to boot or the rebuild fails.

## See also

- [call](call.md): invoke a named tool on a bundle, including the tool collections `run` refuses.
- [serve](serve.md): keep an agent up on a URL instead of one run at a time, sharing the same bundle resolution and boot report.
- [build](build.md), [import](import.md): produce the bundle `run` boots, and set its `MAIN`, `EGRESS`, and declared inputs.
- [VESSELFILE.md](VESSELFILE.md): the `MAIN`, `BUDGET`, `SECRETS`, `ENV`, and `EGRESS` directives this command reads.
- [REASONER.md](REASONER.md): the reasoning harness a `MAIN` agent runs, and the messages contract `run` sends it.
- README: [Give it a brain](../README.md#give-it-a-brain) for building a runnable agent, and [Cage it](../README.md#cage-it) for the wrapping basics.
