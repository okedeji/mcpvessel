# import

Turn an existing MCP server into a caged agent. `import` writes a Vesselfile that installs and launches the server, then builds it into a `.agent` bundle you can run, serve, push, and depend on. With `--reasoning` it goes further and composes several servers into one agent that reasons over their tools.

```
mcpvessel import SOURCE... [flags]
```

`import` is a convenience over `build`: it generates the Vesselfile you would otherwise write by hand, then runs an ordinary build against it. The Vesselfile it writes is yours. Edit it and rebuild whenever you want.

## What a SOURCE can be

Each SOURCE names a server to wrap. Four forms are accepted.

**An MCP Registry name** (reverse-DNS): `io.github.github/github-mcp-server`, `com.example/server`. `import` resolves the name against the official MCP Registry, reads the server's entry, and picks the first package it can wrap. It refuses a server that publishes only a remote URL (a hosted endpoint is not something a cage can contain) and a server whose only package is a type it does not support.

**A direct package coordinate**, when you already know the package:

- `npm:@scope/name` or `npm:name@version`
- `pypi:name` or `pypi:name==version`
- `oci:ghcr.io/org/image:tag` or `oci:image@sha256:...`

The prefix picks the ecosystem. A coordinate skips the registry lookup and wraps the package directly.

**An OCI image with an inline launch command**, for an image whose start command `import` cannot infer:

```
"oci:ghcr.io/acme/mcp-slack:1.2 -- mcp-slack --stdio"
```

Everything after ` -- ` is the command the container runs. The whole SOURCE is one shell argument, so it is quoted.

**A published mcpvessel bundle.** If a SOURCE resolves to a bundle someone already built and pushed with mcpvessel (rather than a plain package or image), there is nothing to wrap. `import` pulls it into your store and tells you to serve or run it directly. `--dir`, `--tag`, and `--entrypoint` do not apply to a finished bundle and are ignored with a note.

You can pass several SOURCEs at once. Each wraps into its own bundle, in its own directory. `--tag` and `--dir` name a single bundle, so they are rejected with more than one SOURCE unless you are composing with `--reasoning`.

## How a server becomes a bundle

For each SOURCE, `import`:

1. **Resolves it** to a package coordinate (a registry name is looked up; a coordinate is used as is).
2. **Generates a Vesselfile** in a directory (`./<name>` by default, or `--dir`). The Vesselfile depends on the ecosystem:
   - **npm**: `FROM node:22-slim`, `RUN npm install -g <pkg>`, and an offline launcher as the entrypoint. The launcher (`npm-entry.sh`, written beside the Vesselfile) resolves the package's bin from its own `package.json` and execs node on it. This replaces `npx`, which pings the registry on every start and would hang in a cage with no network.
   - **PyPI**: `FROM python:3.12-slim`, `RUN pip install --no-cache-dir <pkg>`, and the package as the entrypoint.
   - **OCI**: the image itself is the base. Its launch command is required, since wrap cannot infer it (see [Launch command](#launch-command-for-oci-images)).
   - Base images are pinned to a concrete tag, so a rebuild produces the same bytes instead of drifting when the upstream base moves.
3. **Stages the bridge.** The server speaks stdio; mcpvessel reaches a served agent over HTTP. So the entrypoint is not the server directly but the `mcpvessel` bridge wrapping it (`./mcpvessel mcp-bridge -- <launch>`). As a root the bridge execs the server over stdio; as a `USES` sub-agent it serves HTTP and forwards. The entrypoint is written in exec form (a JSON array, no shell), so a wrapped server can sit on a distroless base.
4. **Builds the bundle** into your store. The build boots the server once to list its tools (introspection, see [Inputs](#inputs-a-server-needs-to-start)), seals a manifest, and writes a content-addressed `.agent`. With `--tag` the bundle is indexed under that reference; without one it is addressable only by its content hash, which the build prints.

After a successful import you have two things: the editable source directory, and the built bundle in your store. Edit the Vesselfile (tighten its egress, add an input) and rebuild the directory to pick up the change.

## Wrap or compose

Without `--reasoning`, each SOURCE becomes an independent **tool collection**: a caged server whose tools you serve or depend on directly. It has no `MAIN`, so you `call` its tools by name rather than `run` it.

With `--reasoning`, every SOURCE is composed under **one reasoning agent**. `import` wraps each source as a tool collection (or reuses an existing wrapper, see [Reuse](#reuse-with---reasoning)), then generates a reasoning agent that `USES` each collection and answers a prompt by running an LLM tool-use loop over all their tools. The reasoning agent has a `MAIN` (`respond`), so you `run` it with a goal.

`--reasoning` requires `-t` (a versioned, namespaced tag like `@me/oncall:0.1`): the agent and each tool collection it composes all need concrete references. Shape the agent's behavior with `--prompt` / `--prompt-file`, pin its model with `--model`, or swap the reasoning loop with `--reasoner`. See [give it a brain](../README.md#give-it-a-brain) for the walkthrough and [REASONER.md](REASONER.md) for the harness contract.

## Inputs a server needs to start

Many servers need a key or a config value just to boot (a SaaS server with no key often exits immediately). Introspection has to boot the server to list its tools, so those inputs must be available at import time.

- **`--secret NAME`** grants a secret the server needs to start. The value is resolved from your environment or the mcpvessel secret store, never the command line, so it stays out of your shell history and the process table. Store one first with `mcpvessel secrets set NAME`.
- **`--env KEY=VALUE`** supplies a plain config value. `--env KEY` (no value) passes it through from your environment.
- **`--secret-file`** and **`--env-file`** read many at once, one `NAME=VALUE` per line.

`import` reads the server's declared inputs from its registry entry and writes them into the Vesselfile: a secret becomes a `SECRETS` line (injected at run time, never baked into the image), a plain value becomes `ENV`. It then prints which inputs the agent declares, marks the ones you supplied, and shows how to pass the rest at run time. A required input you do not supply fails the introspection boot with a clear error.

## Egress

A wrapped server is deny-default: no outbound network unless you allow it.

- **`--egress host,host`** allows hosts for every server in the import.
- **`--egress agent:host,host`** scopes hosts to one server, matched by its generated directory name, so a batch can give each server its own allowance.

The hosts land in the Vesselfile as an `EGRESS allow:` line, so they travel with the bundle. You do not have to know the hosts at import time, though. Leave `--egress` off and the cage starts deny-default: the first time the server reaches a new host at run time, the connection is held and you approve it with [egress](egress.md) allow, which remembers it for next time. `--egress` here just bakes a known allowance into the bundle up front.

## Launch command for OCI images

npm and PyPI packages have a launch command wrap can derive. An OCI image does not, so `import` finds it in one of three ways, in order:

1. An **inline launch** in the SOURCE (`"oci:img -- cmd args"`).
2. **`--entrypoint "cmd args"`**.
3. The image's **own baked launch command**. With neither of the above, `import` pulls the image and reads its `ENTRYPOINT` and `CMD`, so you need not know the in-container command. If the image declares none, the wrap fails asking you to pass `--entrypoint`.

## Reuse with --reasoning

When you compose with `--reasoning`, `import` avoids rebuilding a server you have already wrapped. Before wrapping a source fresh, it looks for an existing tool collection of the same server, both in your local store and on the MCP Registry, and reuses it if found. This keeps a shared server (a fetch tool, say) as one bundle across the agents that compose it, rather than a fresh copy per agent.

**`--no-reuse`** turns this off and wraps a fresh tool collection every time.

## Flags

| Flag | Meaning |
| --- | --- |
| `-t`, `--tag REF` | Name the built bundle (`@org/name:version`). Required with `--reasoning`, where it names the reasoning agent. One bundle only, so not valid with multiple SOURCEs (except `--reasoning`, which composes them into one). |
| `--dir PATH` | Directory to write the generated Vesselfile into. Default `./<name>`. Single SOURCE only. |
| `--entrypoint "CMD"` | Launch command for an OCI image. Single SOURCE only; for a batch, use the inline `"oci:img -- cmd"` form. |
| `--reasoning` | Compose every SOURCE under one reasoning agent instead of wrapping each on its own. |
| `--model PROVIDER/MODEL` | With `--reasoning`, pin the agent's model. Default defers to your configured provider. |
| `--prompt TEXT` | With `--reasoning`, the agent's system prompt, appended to the harness's built-in one. Mutually exclusive with `--prompt-file`. |
| `--prompt-file PATH` | With `--reasoning`, read the system prompt from a file, for a multi-line prompt. |
| `--reasoner PATH` | With `--reasoning`, use a custom reasoning harness `.py` instead of the built-in one. |
| `--no-reuse` | With `--reasoning`, wrap a fresh tool collection instead of reusing an existing wrapper of the same server. |
| `--secret NAME` | Supply a secret the server needs to start, resolved from your environment or the secret store. Repeatable. |
| `--secret-file PATH` | Read secret values (`NAME=VALUE` per line) from a permissions-restricted file. |
| `--env KEY=VALUE` | Supply an env value the server needs to start, or `KEY` to pass it through from your environment. Repeatable. |
| `--env-file PATH` | Read env values (`KEY=VALUE` per line) from a file. |
| `--egress HOSTS` | Hosts a server may reach: `host,host`, or `agent:host,host` to scope one of several. Repeatable. Default is deny-default with approval at run time. |
| `--force` | Overwrite an existing generated Vesselfile instead of refusing. |
| `--progress auto\|plain\|tty` | Build progress output. Default `auto`. |

## What lands on disk

**A single import** writes `./<name>/` containing the `Vesselfile`, the `mcpvessel` bridge binary, and (for npm) the launcher script. The built bundle goes into your store, tagged if you passed `-t`.

**A `--reasoning` import** writes `./<agent>/` containing an `agent/` directory (the reasoning agent's Vesselfile, `reasoner.py`, and `system_prompt.txt` if you set a prompt) and one `<name>-tools/` directory per composed source. Each tool collection and the agent are built and tagged.

If a generated directory already exists, `import` refuses rather than clobber it, unless you pass `--force`. A build that fails removes the directory it just created so a retry starts clean.

## Examples

```sh
# Wrap a registry server and name it.
mcpvessel import io.github.github/github-mcp-server -t @me/github:0.1 --secret GITHUB_PERSONAL_ACCESS_TOKEN

# Wrap a PyPI package directly, no registry lookup.
mcpvessel import pypi:mcp-server-time -t @me/time:0.1

# Wrap an OCI image, giving its launch command.
mcpvessel import oci:ghcr.io/acme/mcp-slack:1.2 --entrypoint "mcp-slack --stdio" -t @me/slack:0.1

# Wrap several servers, each its own bundle.
mcpvessel import npm:server-github pypi:mcp-server-time

# Import without guessing hosts: run it deny-default and approve hosts as they come up.
mcpvessel import io.github.github/github-mcp-server --secret GITHUB_PERSONAL_ACCESS_TOKEN

# Compose two servers into one reasoning agent with a role.
mcpvessel import io.github.getsentry/sentry-mcp io.github.brave/brave-search-mcp-server \
  --reasoning -t @me/oncall:0.1 --prompt "You are an on-call SRE." \
  --secret SENTRY_ACCESS_TOKEN --secret BRAVE_API_KEY
```

## Notes

- Importing a server does not vet its code. It generates a cage around what the server can reach. What runs inside is still up to the package.
- A wrapped tool collection has no `MAIN`. Use `mcpvessel call <bundle> <tool>` to invoke a tool, not `run`.
- The bridge binary staged in the directory is your host's; the runtime injects the correct one for the target architecture at build time, so a bundle built on one machine runs on another.
- To change a wrapped server later, edit its Vesselfile and `mcpvessel build <dir>`, or re-import. A published bundle you pulled is sealed; re-import from source to change it.

## See also

- [build](build.md): what `import` runs under the hood, and how to build a hand-written Vesselfile.
- [serve](serve.md), [run](run.md), [call](call.md): using a bundle once it is built.
- [REASONER.md](REASONER.md): the reasoning harness `--reasoning` writes, and its environment contract.
- [VESSELFILE.md](VESSELFILE.md): the directives `import` generates, for when you edit by hand.
