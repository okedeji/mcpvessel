# build

Build an agent bundle from a directory holding a `Vesselfile` and its source. `build` hashes the tree, resolves and locks any `USES` dependencies, boots the agent once to read its tools, seals a manifest, and writes a content-addressed `.agent` into your store. The result is a bundle you can run, serve, push, and depend on. `import` runs `build` under the hood; this is the command for a Vesselfile you wrote or edited by hand.

```
mcpvessel build [PATH] [flags]
```

`PATH` is the source directory and defaults to the current directory. It must contain a `Vesselfile` at its root. At most one PATH is accepted.

## What a build produces

Every build writes one bundle into your local store under `~/.mcpvessel`, a gzip-tar with a `manifest.json` at the root and a `files/` directory holding every source file (VCS metadata and the output itself excluded). The bundle's name in the store is its **content hash**: a sha256 over the canonical source tree, so the same input bytes always land at the same path and an unchanged tree rebuilds to the identical bundle.

- **Without `-t`** the bundle is addressable by that hash alone. The build prints the hash and a tip on how to name or export it.
- **With `-t`** the store also indexes the bundle under your reference (`@org/name:version`), so later `run`, `serve`, and `push` find it by name. The tag is a pointer to the same content-hashed bundle, not a second copy.
- **With `-o`** the build additionally writes a portable copy of the `.agent` to the path you give, for handing the bundle around outside the store.

`-t` is parsed before any of the expensive work, so a malformed reference fails immediately rather than after an image build.

## Staging the bridge

A wrapped server speaks stdio, but mcpvessel reaches a served agent over HTTP, so a wrapped Vesselfile's `ENTRYPOINT` runs the `mcpvessel mcp-bridge` companion rather than the server directly. That companion is a Linux binary that has to be present in the source tree before the tree is hashed and sealed.

`import` stages it for the agents it generates. `build` does the same for a hand-written agent, so you do not have to copy a Linux binary in yourself. Before hashing, `build` parses the Vesselfile, and if its `ENTRYPOINT` runs `mcp-bridge` it locates this host's companion (`mcpvessel-linux-<arch>`, the same binary baked into the runtime image) and writes it into the directory as `mcpvessel`.

- If the binary is already staged and matches, nothing happens.
- If a staged copy no longer matches this host's companion (stale, or built for another architecture), it is **replaced**, with a note on stderr. A wrong binary would otherwise bake into the bundle and its hash.
- A Vesselfile that does not parse, or whose entrypoint does not run the bridge, is left untouched.

Staging runs before the hash, so the sealed files carry the exact companion the introspection boot reuses.

## Introspection: booting the agent to read its tools

By default `build` introspects the agent. It builds the agent's image, boots it briefly, and asks its MCP server for its tools with a single `tools/list`, then writes each tool's name, description, and JSON schema into the bundle's catalog. This is metadata only: no tool is ever called and the agent's LLM is never invoked, only the agent's own server startup runs. The `tools/list` round-trip is bounded at 60 seconds; the boot handshake itself is not, since timing it out would kill the session about to be read.

Introspection needs the runtime (a Linux VM on macOS, see [Requirements](../README.md#requirements)). The image it builds is content-addressed from the same source hash a later `run` derives, so the image built here is the one that run reuses rather than rebuilds.

A boot or `tools/list` failure is **fatal**: an agent that will not start should not ship. The error suggests supplying a key or config with `--secret` / `--env`, but note those flags live on `import`, not `build` (see [Introspecting a server that needs inputs](#introspecting-a-server-that-needs-inputs)).

After a successful introspection the build prints how many tools it read and warns, without blocking, about any **public** tool (the `MAIN`, or a name in `EXPOSE`) that has no description. Private tools are exempt.

**`--no-introspect`** skips the boot entirely and ships the declared-only catalog. No runtime is needed. `USES` resolution still runs. Use it to build on a host without the runtime, or when you do not need enriched tool metadata in the bundle.

**`--no-cache`** rebuilds the introspection image from scratch, ignoring both BuildKit's layer cache and any already-built image. It only matters while introspecting; with `--no-introspect` no image is built.

## Introspecting a server that needs inputs

Many servers need a key or a config value just to boot, so introspection has to have those inputs at build time or the boot fails. The introspection boot draws from an env pool and a secret pool, each scoped to what the Vesselfile declares.

The `build` command itself has no flag to fill those pools. They are populated by `import`, which does expose `--secret` and `--env` (and their file forms) and passes them through to the same build path. So for a server that needs inputs to start, either introspect it through `import`, or run `build --no-introspect` and let the tool metadata fill in on a later boot. The introspection error names this, pointing you at `--secret NAME` / `--env KEY=VALUE` and at `mcpvessel secrets set` for storing a secret first.

## USES: locking dependencies

When the Vesselfile has `USES` dependencies, `build` resolves each one before packaging and locks it into the manifest, so the bundle pins the exact bytes it was built against.

1. **Resolve each tag to a digest.** For every `USES`, `build` finds the dependency and records its OCI digest. Resolution is **store-first**: it checks your local store before the registry, so a parent builds against a sibling you built with `-t` and never pushed. The digest it locks is the deterministic digest a `push` would produce, so the lock stays valid after one. A fully local graph resolves with no registry and no credentials; the registry is reached, and its error surfaced, only when a dependency is not in the store.
2. **Walk the graph to reject cycles.** After locking, `build` walks the transitive `USES` graph depth-first and fails on any cycle. `-t` names the agent being built (the `ParentKey`), so a dependency that loops back to it is caught by name. Without `-t` a loop back to the parent cannot be named, but cycles among the dependencies themselves are still caught. Pulling a sub-agent for the walk can reach the registry.

A leaf agent with no `USES` skips all of this and builds fully offline.

**`--skip-cycle-check`** skips only the transitive walk, on a graph you already trust. Digests are still resolved and locked; a cycle you skip past surfaces at first run instead.

## Seeding the pull cache

After sealing the bundle, `build` computes its OCI digest and writes the bundle into your local pull cache under that digest. This is a local write that needs no registry credentials. It means a parent agent that `USES` this one resolves it from the cache immediately, without a `push` to a registry first. Build a dependency and its parent locally and the parent finds the child on its own.

## Progress output

While introspecting, BuildKit's own output owns the screen and the packaging steps stay quiet. Otherwise the build renders a three-step progress line: parsing the Vesselfile, hashing the source tree, sealing the bundle.

**`--progress`** picks the renderer: `auto` (the default) adapts to whether output is a terminal, `plain` prints one line per step for logs and pipes, `tty` forces the live renderer even when piped.

**`--json`** emits one JSON object on stdout with the built `ref`, `hash`, `size_bytes`, and `duration_ms`, and routes all progress to stderr so stdout stays pure JSON. It lets an agent read the built ref straight off stdout rather than scrape the human "Successfully built ..." line.

## Flags

| Flag | Meaning |
| --- | --- |
| `-t`, `--tag REF` | Reference for the built bundle (`@org/name:version`). Indexes the store bundle by name so `run`, `serve`, and `push` find it, and anchors `USES` cycle detection as the parent. Parsed up front, so a bad ref fails before the build. Without it the bundle is addressable by content hash only. |
| `-o`, `--output PATH` | Also write a portable copy of the `.agent` to this path. The bundle still goes into the store; this is an extra copy. |
| `--no-introspect` | Skip booting the agent to enrich the catalog. Ships the declared-only catalog and needs no runtime. `USES` resolution still runs. |
| `--no-cache` | Rebuild the introspection image from scratch, ignoring cached and already-built images. Only relevant while introspecting. |
| `--skip-cycle-check` | Skip the transitive `USES` cycle walk. Digests are still resolved and locked; a cycle surfaces at first run instead. |
| `--progress auto\|plain\|tty` | Build progress output. Default `auto`. Suppressed while introspecting, where BuildKit owns the screen. |
| `--json` | Emit one JSON object (`ref`, `hash`, `size_bytes`, `duration_ms`) on stdout, with all progress sent to stderr. For an agent reading the built ref programmatically. |

## Examples

```sh
# Build the current directory into the store, addressable by content hash.
mcpvessel build .

# Build a directory and name it, so run/serve/push find it by reference.
mcpvessel build ./my-agent -t @okedeji/researcher:0.1

# Build, name, and also write a portable copy.
mcpvessel build . -t @okedeji/researcher:0.1 -o researcher.agent

# Build without booting the agent: declared-only catalog, no runtime needed.
mcpvessel build . --no-introspect
```

## Notes

- The content hash is over the source tree, not the built image. Two builds of the same files produce the same hash and the same bundle. Changing a byte in the tree, including a re-staged bridge binary, changes the hash.
- A build that fails after staging the bridge leaves the staged `mcpvessel` binary in the source directory. It is the correct companion for this host and a re-run reuses it.
- `--no-introspect` ships a catalog built only from what the Vesselfile declares. It does not verify that the agent boots. The first `run` or `serve` is where a broken agent shows itself.
- `build` has no `--secret` / `--env` flags. A server that needs inputs to boot must be introspected through `import`, or built with `--no-introspect`. The introspection error text mentions `--secret` / `--env` regardless of which command you ran.
- `--tag` requires a version. A reference with no version is rejected when the store tries to index it.
- `--json` prints the built bundle as a single JSON object (`ref`, `hash`, `size_bytes`, `duration_ms`) and pushes all progress to stderr, so stdout carries only that object. An agent can read the ref without matching the "Successfully built ..." line.
- A declared `EVAL` suite is validated before the image build. Its path must stay inside the source directory and the suite file must exist and parse, or the build fails early. A malformed Vesselfile is reported by the packaging step, not here.

## See also

- [import](import.md): generate a Vesselfile from an existing MCP server, then build it. Everything on this page runs under `import`.
- [Vesselfile](VESSELFILE.md): the directives `build` reads, including `USES`, `EXPOSE`, `MAIN`, and `EVAL`.
- [serve](serve.md), [run](run.md), [call](call.md): using a bundle once it is built.
- [push](push.md) and [Ship it](../README.md#ship-it): publishing a built bundle to a registry.
- [Requirements](../README.md#requirements): the runtime introspection needs, and its one-time setup.
