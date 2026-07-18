# Vesselfile

The Vesselfile is the declarative manifest at the root of every agent's source tree. It says which image the caged server starts from, the command that launches it, which tools it shows, what inputs and network it needs, and, for an agent, how it reasons. `import` writes one for you from an npm, PyPI, or OCI source; `build` reads it to produce the content-addressed bundle. You edit it by hand when you want to change what the generated defaults gave you.

It is line-oriented and looks like a Dockerfile on purpose, but it is not one. Each non-blank line is a directive keyword followed by its arguments. The keyword is case-insensitive (`FROM` and `from` both parse); everything after it is taken as written. Blank lines and lines beginning with `#` are ignored. There is no line-continuation syntax, so each directive is a single line.

```
FROM node:22-slim
RUN npm install -g @acme/weather-mcp
ENV WEATHER_API_KEY
SECRETS WEATHER_API_KEY
EGRESS allow:api.weather.example
EXPOSE get_forecast get_current
ENTRYPOINT ["weather-mcp", "--stdio"]
```

Two directives are required and the parser rejects a file without them: `FROM` and `ENTRYPOINT`. Everything else is optional and takes a Go zero value when absent. A file that names neither a `MAIN` tool nor any `USES` is a plain caged server, a tool collection. Add `MAIN` (and usually `MODEL`) and it becomes a reasoning agent.

## A theme: author proposes, operator disposes

Several directives record what the author wants, not what the run enforces. `BUDGET` and `RESOURCES` are advisory: the real per-run spend cap is the operator's `--budget` at `run`, `call`, or `serve`, and the real cpu/memory/pids cap is what the operator set with `config resources`. `MODEL` names a provider and model, but the LLM gateway resolves the provider at run time against the operator's configured keys. `ENV KEY=value` sets a default the operator can override. Keep this split in mind: the Vesselfile declares intent and shape; the person running the bundle supplies the secrets, the network grants, and the enforced caps. That is what lets a bundle be shared without shipping anyone's keys or trusting its author with your machine.

## Building the image

### FROM

```
FROM <oci-image-reference>
```

The base image the caged server is built on. Required, and may appear once. `import` picks it for you: `node:22-slim` for an npm source, `python:3.12-slim` for PyPI, or the image itself for an OCI source. Any reference a container registry serves works, including a distroless or `scratch`-derived base, as long as your `ENTRYPOINT` can run on it.

### RUN

```
RUN <command>
```

A build-time command, run in the image as the bundle is built. Repeatable, and the lines run in order. This is where a server gets installed: `RUN npm install -g <pkg>`, `RUN pip install <pkg>`. Each `RUN` is one shell command line. Nothing here runs at agent runtime; it only shapes the image.

### ENTRYPOINT

```
ENTRYPOINT <command line>
ENTRYPOINT ["prog", "--flag", "arg"]
```

The command that starts the MCP server over stdio. Required, and may appear once. Two forms, the same distinction a Dockerfile draws:

- **Shell form** is a bare command line. It runs through `sh -c`, so shell features (variable expansion, pipes) work, but the base image must have a shell.
- **Exec form** is a JSON array of strings, marked by the leading `[`. It runs the program directly with no shell, which is what lets an agent sit on a distroless or `scratch` base that has no `sh`. The array must be valid JSON and non-empty.

`import` writes the exec form for an OCI source so the bridge is invoked directly, and a shell-form line for npm and PyPI launchers.

## What the server shows

### MAIN

```
MAIN <tool-name>
```

Names the single tool that is the agent's reasoning entry point, the one `run` and `call` invoke and `serve` fronts as `/agents/<name>`. Present it and the bundle is an agent; omit it and the bundle is a tool collection whose tools a client drives directly. At most one `MAIN`, one tool name. The parser only checks the shape; whether that tool actually exists is a build-time introspection check, so a typo here surfaces when you `build`, not when you save.

### EXPOSE

```
EXPOSE tool_a tool_b
EXPOSE tool_c, tool_d
```

The tools that are publicly callable from outside the cage. Repeatable, and names may be separated by spaces or commas; duplicates across lines are merged, not rejected. `EXPOSE` is the boundary a parent sees over `USES`: a sub-agent contributes exactly the tools it exposes, minus anything the parent denies. With no `EXPOSE`, a tool collection still serves its tools, but a parent composing it over `USES` has nothing it is promised to keep.

## Composing other agents

### USES

```
USES @org/name:version
USES PUBLIC @org/name:version
USES @org/name:version DENY tool_a, tool_b
USES host/org/name:version
```

Declares a published agent this one depends on, pulled and caged as a child at build time. Repeatable. The reference is either `@org/name:version` on your default registry or `host/org/name:version` pinning an explicit registry host, and it must carry a concrete version tag. `latest` is rejected outright, because a dependency that drifts breaks the content hash and reproducibility.

Two modifiers:

- **PUBLIC**, before the reference, marks the sub-agent reachable by clients of the parent, not only internally. Without it a `USES` child is a private helper the parent reasons with but does not re-expose.
- **DENY**, after the reference, lists tools of that sub-agent the parent refuses. With no `DENY` the parent accepts everything the child `EXPOSE`s. The list is comma or space separated.

### BAN

```
BAN @org/name
BAN @org/name ONLY tool_a, tool_b
```

Forbids an agent, or specific tools of it, anywhere in this agent's dependency subtree. Repeatable. Where `DENY` cuts one edge, `BAN` is inherited: it catches the named agent however deep a transitive `USES` reached it. For that reason `BAN` names an agent by `@org/name` with no version, so it holds whatever version a dependency happened to pin; a version here is a parse error. `ONLY` narrows the ban to the listed tools, leaving the agent otherwise running. This is the guardrail for composing agents you did not author: pull a tree, ban the capability you do not want, and it stays banned no matter which layer tried to reach it.

## Runtime inputs

### ENV

```
ENV KEY=value
ENV KEY
ENV KEY?
```

Environment variables the server reads at runtime. Repeatable. Three shapes:

- **`ENV KEY=value`** bakes an author default the operator can still override at run time.
- **`ENV KEY`** with no value declares a required input. Nothing is baked in; the run fails closed unless the operator supplies it with `--env KEY=...` or `--env-file`.
- **`ENV KEY?`** marks a value-less input optional: used if supplied, absent (not an error) otherwise, for a capability the server can run without.

Keys beginning with `VESSEL_` are rejected. That prefix is reserved for the wiring mcpvessel injects into the cage (gateway URLs, sub-agent addresses), and letting a Vesselfile set it would let a bundle rewrite its own plumbing.

### SECRETS

```
SECRETS KEY_ONE KEY_TWO
SECRETS OPTIONAL_KEY?
```

The secret keys the server needs injected at runtime. Repeatable, comma or space separated. A secret differs from an `ENV` value in where it comes from and how it is handled: the operator supplies it from their secret store or `--secret`, it is scoped so it only reaches the server that declares it, and it is kept out of logs and the bundle. A trailing `?` marks a secret optional, the same fail-open rule as `ENV KEY?`. `SECRETS` is a declaration of need; the value never lives in the Vesselfile.

### EGRESS

```
EGRESS deny-default
EGRESS allow:api.example.com,cdn.example.com
```

The server's network policy, baked into the bundle. May appear once. Two forms, and their absence is a third, meaningfully distinct case:

- **`EGRESS allow:host,host`** grants exactly those hosts and nothing else, the author's baseline.
- **`EGRESS deny-default`** is hard isolation: no egress proxy runs, the server has no outbound path, and any attempt fails immediately. Use it for a pure-compute tool where any outbound connection is a red flag.
- **No `EGRESS` directive** is deny-default *with interactive approval*: the server starts reaching nothing, but the first time it reaches a new host the connection is held and the operator approves it with [egress](egress.md) allow rather than the call hard-failing.

Either way the operator can widen it per run with `--egress`, persist hosts with `config egress`, or approve them live. `EGRESS` is the author's starting point, not the last word.

## Reasoning

### MODEL

```
MODEL provider/model-name
```

The provider and model an agent reasons with, for example `MODEL anthropic/claude-opus-4-8` or `MODEL openai/gpt-5.5`. May appear once. It is advisory: the parser accepts any provider name, and the LLM gateway resolves it at run time against the operator's configured providers and keys, so the model key never enters the cage. Only meaningful alongside `MAIN`; a tool collection does not reason.

### BUDGET

```
BUDGET 5.00
```

An advisory per-run spend cap in US dollars, for example `BUDGET 5.00` or `BUDGET 0.25`. May appear once, must be positive, and is stored internally as micro-USD so it accumulates without floating-point drift (up to six decimal places). It is a hint the author ships; the enforced cap is the operator's `--budget`. If neither is set the run has no spend ceiling, so treat `BUDGET` as a sane default rather than a guarantee.

### RESOURCES

```
RESOURCES cpu=2 mem=2g pids=256
```

An advisory hint for the cpu, memory, and process caps the cage would like. May appear once, needs at least one of `cpu=`, `mem=`, `pids=`. `cpu` is a positive number of cores, `mem` a nerdctl size like `512m` or `2g`, `pids` a positive process count. Like `BUDGET` these numbers are never applied directly. The enforced caps come from the operator's `config resources`; `RESOURCES` documents what the agent expects to run comfortably.

## Metadata and evaluation

### META

```
META name "Weather agent"
META description "Forecasts and current conditions."
```

A key and value carried into the bundle for registry discovery. Repeatable, one pair per line. The value is everything after the key and may be wrapped in double quotes, which are stripped. This is what `register` and a registry listing read to describe the bundle; it does not affect how the agent runs.

### EVAL

```
EVAL evals/
```

A path, relative to the source tree, to the eval suite for this agent. May appear once. `eval` runs it, and `push --with-evals` and `build` read it so a published bundle can carry its own tests. It is inert at agent runtime.

## Directive summary

| Directive | Repeatable | Purpose |
| --- | --- | --- |
| `FROM` (required) | no | Base OCI image the caged server builds on. |
| `ENTRYPOINT` (required) | no | Command that starts the MCP server; shell form or JSON-array exec form. |
| `RUN` | yes, ordered | Build-time command, typically installing the server. |
| `MAIN` | no | The reasoning-entry tool; its presence makes the bundle an agent. |
| `EXPOSE` | yes, merged | Tool names callable from outside the cage. |
| `USES` | yes | A published sub-agent dependency, with optional `PUBLIC` and `DENY`. |
| `BAN` | yes | An agent or its tools forbidden subtree-wide, by name without a version. |
| `ENV` | yes | Author default (`KEY=value`), required input (`KEY`), or optional input (`KEY?`). |
| `SECRETS` | yes | Secret keys to inject at runtime; `?` marks one optional. |
| `EGRESS` | no | Baseline network policy: `deny-default` or `allow:hosts`. |
| `MODEL` | no | Advisory provider/model for reasoning. |
| `BUDGET` | no | Advisory per-run USD spend cap. |
| `RESOURCES` | no | Advisory cpu/mem/pids hint. |
| `META` | yes | Discovery metadata carried into the bundle. |
| `EVAL` | no | Path to the agent's eval suite. |

## A full example

A reasoning agent that composes a published weather collection, reasons over it with a model, keeps a tight network and spend baseline, and refuses one capability anywhere in its tree:

Comments go on their own line, the way they do here:

```
# Build on a stock Python base and install the reasoner.
FROM python:3.12-slim
RUN pip install mcpvessel-reasoner

# Discovery metadata for the registry.
META name "Trip planner"
META description "Plans day trips from weather and local search."

# How it reasons, plus its advisory spend and resource baseline.
MODEL anthropic/claude-opus-4-8
BUDGET 2.00
RESOURCES cpu=2 mem=1g

# Compose two published collections; drop one tool, ban one subtree-wide.
USES PUBLIC @acme/weather:1.4.0
USES @acme/places:0.3.0 DENY delete_review
BAN @acme/analytics ONLY track_event

# The reasoning entry point, and the only tool exposed to callers.
MAIN plan_trip
EXPOSE plan_trip

# Runtime needs: one secret, one host.
SECRETS BRAVE_API_KEY
EGRESS allow:api.search.brave.com

ENTRYPOINT ["mcpvessel-reasoner"]
```

## Parsing rules, in one place

- The directive keyword is case-insensitive; its arguments are not.
- Blank lines and `#` comment lines are skipped. There is no inline `#` comment after a directive and no line continuation.
- `FROM` and `ENTRYPOINT` are required; a file missing either fails to parse.
- Directives marked once above (`FROM`, `ENTRYPOINT`, `MODEL`, `MAIN`, `BUDGET`, `RESOURCES`, `EGRESS`, `EVAL`) error if declared twice.
- An unknown directive keyword is a parse error naming the line, so a typo fails fast rather than being silently ignored.

## See also

- [import](import.md): generates the Vesselfile these directives document, from an npm, PyPI, or OCI source.
- [build](build.md): reads the Vesselfile and turns the source tree into a content-addressed bundle.
- [inspect](inspect.md): prints the resolved directives of a built bundle.
- [tree](tree.md): shows the `USES` graph and where `BAN` and `DENY` cut it.
- [REASONER.md](REASONER.md): the harness a `MAIN` agent runs, and the environment contract it expects.
- [ARCHITECTURE.md](ARCHITECTURE.md): the cages, brokers, and networks these directives configure.
