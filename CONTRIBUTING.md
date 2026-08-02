# Contributing

Thanks for looking. mcpvessel is early and solo-maintained, so the most useful
contributions right now are bug reports with a reproduction, and small focused
PRs. Before a large change, open an issue so we can agree on the shape first.

## Building and testing

```sh
make ci            # fmt-check, vet, lint, test, build (run this before build)
make build         # the host CLI -> bin/mcpvessel
make build-linux   # the in-VM companion binary (needed to actually run agents)
```

You need Go 1.26+. `make lint` needs
[golangci-lint](https://golangci-lint.run/welcome/install/). Running agents
needs a container runtime: containerd + buildkit + nerdctl on Linux, or the
bundled Lima VM on macOS (`mcpvessel init` sets it up).

The test suite is hermetic. It uses `httptest` servers and fakes rather than a
real daemon or VM, so `make test` passes offline with no containers running.
Keep it that way: a test that needs a live runtime does not belong in the unit
suite.

## Live testing against a real runtime

The unit suite covers logic, but some changes (the container topology, the
egress proxy, image builds) can only be exercised for real, in a full caged run.
Never do this against your own `~/.mcpvessel`: a fresh daemon's startup sweep
tears down daemon-labeled containers, so pointing a dev build at your real VM
would kill any agents you are serving. Use the isolated harness instead, which
gives the dev runtime its own `VESSEL_HOME`, its own Lima VM, and its own daemon
socket, all separate from the real install (`VESSEL_HOME` isolates the VM too, so
nothing is shared):

```sh
scripts/devvm.sh rebuild        # build host + in-VM binaries from local code
scripts/devvm.sh up             # provision the dev VM once, then reuse it
scripts/devvm.sh build ./dir -t @dev/x:0.1   # any mcpvessel command passes through
scripts/devvm.sh run @dev/x:0.1 "hello"
scripts/devvm.sh down           # stop the VM, keep the disk (next 'up' is fast)
scripts/devvm.sh clean          # delete the dev VM and its state entirely
```

The first `up` provisions a VM (a couple of minutes); later ones reuse it in
seconds. `scripts/devvm.sh help` lists everything. Override the location with
`MCPVESSEL_DEV_HOME`; it defaults to `~/.mcpvessel-dev`, outside the repo.

## Pull requests

- Run `make ci` before opening a PR; a red pipeline will not merge.
- Keep the change focused. One concern per PR.
- Match the surrounding code. Look at a neighboring file before inventing a
  pattern.
- Add or update tests for behavior you change.

## Cutting a release

Three things in this repo are versioned independently, because they move at
different speeds and answer different questions:

| What | Version lives in | Released by |
| --- | --- | --- |
| The CLI binary | a `vX.Y.Z` git tag | pushing that tag |
| The skill | `metadata.version` in `internal/clientskill/skills/<client>/SKILL.md` | merging to the default branch |
| The docs MCP server | a `docs-mcp-v*` git tag | pushing that tag |

Getting the relationship between them wrong is the easiest mistake here, so the
rules are spelled out below.

### The CLI binary

```sh
make ci                      # must be green
git push                     # main first: the tag builds from what is on main
git tag vX.Y.Z && git push origin vX.Y.Z
```

The tag triggers `.github/workflows/release.yml`, which runs goreleaser. It
builds the archives, creates a **draft** GitHub release for you to review, and
pushes the updated cask to `okedeji/homebrew-tap`. The cask push happens on the
tag, not when you publish the draft, and only for a non-prerelease tag (one
without a `-`, so `v0.2.0-rc.1` builds a draft without touching the cask).
Review the archives, then publish the draft.

Sub-1.0, treat a minor bump as "new capability" and a patch as "fixes". The
number is user-facing.

Note the Makefile derives the stamped version with `git describe --match 'v*'`.
The `--match` is load-bearing: this repo carries `docs-mcp-v*` tags too, and
without it a local build stamps itself with whichever series was tagged last.

### The skill, and its `requires`

The skill improves faster than the binary and ships to users through the docs
server's `latest_skill` tool, which serves **whatever is on the default branch
right now**. So merging a skill change releases it. There is no tag and no docs
release needed for a skill edit alone.

Two fields in its frontmatter, and both matter:

- **`version`**: bump on every change, or no installed skill will pick it up.
  An agent compares this against its own copy and only updates when yours is
  higher.
- **`requires`**: the minimum CLI version the skill needs. **Bump this the
  moment the skill starts telling an agent to use behavior that does not exist
  in the released binary yet**, then set it to the version you are about to cut.

That second rule is the one that bites. A skill instructing an agent to read a
field, pass a flag, or run a subcommand that its binary does not have produces
confident nonsense: the agent follows the instruction, gets nothing back, and
reports something untrue. An agent whose binary is below `requires` declines the
update and tells the user a binary upgrade is what unlocks it, which is the
outcome you want. Leaving `requires` stale removes that protection.

So a change that touches both code and skill lands in this order: make the code
change, set `requires` to the version you are cutting, merge, then tag that
version. The skill is briefly ahead of every released binary, which is exactly
what `requires` exists to express.

### The docs MCP server

```sh
git tag docs-mcp-v0.1.3 && git push origin docs-mcp-v0.1.3
```

`.github/workflows/docs-mcp.yml` builds it, signs it, pushes it to GHCR, and
registers it in the MCP Registry. Three things to know:

- **Bump the version, always.** The MCP Registry is immutable per version and
  refuses to re-register one it already holds. There is no republishing, only
  superseding.
- **Check the signing key in the run log.** The `Signed: key <fingerprint>` line
  must match the project's stable key. mcpvessel pins a publisher's key on first
  pull, so signing a release with a different key breaks verification for
  everyone who already pulled, and the fix is not to tell them to run
  `trust rm`, it is to correct the key and cut another version.
- **A push to `main` touching `tools/docs-mcp/**` publishes the moving `:edge`
  tag only.** Path filters are not evaluated for tag pushes, so a `docs-mcp-v*`
  tag releases from any commit.

The binary does not pin a docs version. It resolves the registry name to
whatever is current, so publishing a new one reaches existing installs with no
CLI release.

## House style

A few conventions the codebase holds to. New code should match:

- **Comments say what the code cannot.** A comment earns its place by stating a
  constraint, invariant, or trade-off, not by narrating the next line. Density
  follows subtlety: gnarly code gets an explanation, obvious code gets nothing.
- **No em-dashes or en-dashes** in comments, help text, or docs. Use a period,
  comma, colon, or parenthetical.
- **Fail closed.** A missing input, an unparseable config, a policy that cannot
  be evaluated: refuse, do not guess. This is a security tool.
- **Money is integer micro-USD.** No floats for currency, anywhere.
- **Secrets come from stdin or the store, never argv**, so they stay out of the
  process table and shell history.
- User-facing errors are lowercase, specific, and name the remedy.

## Reporting security issues

Not here. See [SECURITY.md](SECURITY.md) for private disclosure.
