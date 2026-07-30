# init

Prepare the mcpvessel runtime on this host, up front, so your first real command does not pay for it. On macOS agents run inside a small Linux VM with a rootless container daemon; the first time anything needs the runtime that VM is created, a Linux image is downloaded, and the daemon is started, which takes 2-5 minutes depending on your connection. `init` does that setup now, on demand, with a visible progress UI, instead of letting it happen inline the first time you `run`, `serve`, or `build`. On Linux it is a near no-op: the host's own containerd and buildkitd are used, no VM.

```
mcpvessel init [flags]
```

`init` takes no arguments. Everything it does is idempotent: run it on a host that is already set up and it confirms the runtime is ready in a second or two, having only checked the VM and started the daemon if it was not already up.

## What init does, in order

Each step is a no-op when it is already satisfied, so a warm host falls straight through to the last line.

1. **Fetch Lima if missing** (macOS only). `init` needs `limactl`, the tool that drives the VM. If none is found it downloads the pinned Lima release, verifies it, and installs it under `~/.mcpvessel/lima`. See [Fetching Lima](#fetching-lima-macos).
2. **Pick the provisioner** for this OS. Linux gets the native path (host containerd and buildkitd, no VM). macOS gets the Lima path, sized from your `~/.mcpvessel/config.json` machine settings. Windows is refused with a message telling you to run inside a WSL2 distro that has containerd and buildkitd.
3. **Recreate the VM** if you passed `--recreate`. See [--recreate](#recreate-rebuild-the-vm).
4. **Bring the runtime up** behind the phase UI, unless it is already up. On macOS that means creating and booting the VM on first run; a VM that is already running skips this entirely. On Linux this step never runs (there is no VM to bring up). See [The setup phases](#the-setup-phases).
5. **Start the daemon.** `init` starts the background daemon if it is not already listening, taking its startup latency now rather than at your first command. This is the point of running `init` on an otherwise warm host. See [Starting the daemon](#starting-the-daemon).
6. **Check the in-VM agent binary** and warn if it is missing. See [The in-VM agent binary](#the-in-vm-agent-binary).
7. **Set up your MCP client**, so its agent can drive mcpvessel itself: install the skill, then serve mcpvessel's own caged docs server and register it with the client. At a terminal `init` asks which client; non-interactively it does this only when you pass `--client`, so scripts and CI are untouched. See [Setting up the client](#setting-up-the-client).
8. **Print `Runtime ready.`** on success.

## Fetching Lima (macOS)

`init` runs on the bundled `limactl` from a Homebrew or direct-download install. When it cannot find one (not bundled next to the binary, not previously fetched under `~/.mcpvessel/lima`, not in a dev tree's `bin/`, not on `PATH`), it fetches the pinned Lima release before going further.

- The version and the per-platform tarball SHA-256s live in one pinned file that both the release build and this auto-fetch read, so a runtime download installs the exact bytes a release would.
- The archive is **SHA-256 verified against the pin before anything is extracted**, so a tampered mirror cannot install a different binary. A mismatch fails with `got ... want ...` and installs nothing.
- The download is roughly 80MB and bounded by a 5-minute timeout. It is extracted to a temp dir and swapped into `~/.mcpvessel/lima` with an atomic rename, so a crash mid-download never leaves a half-installed copy.

This whole step is a no-op off macOS, and a no-op on macOS once `limactl` is present.

## The setup phases

On first run the VM does not exist yet, so `init` provisions it behind a phase UI titled `First-time setup (one-time, takes 2-5 minutes)`. The phases shown are:

- **Lima runtime ready**
- **Preparing Linux VM** (the slow one: downloading and unpacking the Linux image)
- **Booting Linux VM**

The UI taps Lima's output and advances the spinner on known markers (image download, instance creation, guest-agent wait). It watches for substrings rather than exact patterns, so if Lima's log wording shifts between point releases the worst case is a spinner that lingers on the previous phase, not a crash. When Lima reports `READY.` the container daemon and builder inside the VM are already up and their sockets forwarded, so `init` completes the remaining phases at once. The UI closes with a tip that everything is cached under `~/.mcpvessel`, so later runs take seconds.

If the VM is already running, this step is skipped and `init` goes straight to the daemon. On Linux the phase UI never appears.

## Starting the daemon

`init` ensures the background daemon is running. If one is already answering it reuses it; otherwise it spawns a fresh daemon detached from your shell, so it keeps running after `init` returns, with its output appended to `~/.mcpvessel/daemon.log`. The spawned daemon is the same binary and inherits your environment, so its version and its `VESSEL_HOME` (socket and store paths) match the CLI's. `init` waits up to 5 seconds for it to bind its socket; if it does not, the error points you at `~/.mcpvessel/daemon.log`.

## The in-VM agent binary

The runtime needs a Linux mcpvessel binary that runs inside the VM (baked into the broker image). A release bundles it. A from-source tree does not until you build it. After the daemon is up, `init` looks for that binary and, if it is missing, prints a note rather than failing:

```
note: the in-VM agent binary is not built yet; the first run needs it (run 'make build-linux' from source)
```

`init` still reports `Runtime ready.` after this note. The missing binary does not block setup; it would only bite at the first `run`. Surfacing it here means you fix it before then. On an installed release you never see this note.

## Setting up the client

After the runtime is up, `init` sets up the MCP client you pick so its agent can drive mcpvessel for you. This is two things done together:

1. **Install the skill:** a small instructions file that teaches the agent how to drive mcpvessel itself, so you can say "install this MCP server" and the agent imports it into a cage, serves it, registers it, and watches its egress for you. See [skill](skill.md) for the whole agent-driven path.
2. **Serve and register the docs server:** mcpvessel's own documentation and code, shipped as a caged MCP server (`mcpvessel-docs`), served on the fixed loopback address `127.0.0.1:7333` and registered with your client. The agent queries it when it is unsure of mcpvessel's behavior, instead of guessing. Opting in here also persists the choice, so the daemon re-serves the docs server on every later startup and the registered URL keeps working across restarts.

At a terminal `init` prints a short menu of clients and sets up the one you pick. Selecting Claude Code writes the skill to `~/.claude/skills/mcpvessel/SKILL.md`, its personal skills directory picked up across every project, and runs `claude mcp add --scope user mcpvessel-docs --transport http http://127.0.0.1:7333/mcp` to register the docs server. Pressing Enter skips it, and you can set it up later with `mcpvessel init --client claude-code`.

Each client has its own skill, written for that client's tools and register command, kept under `skills/<client>/SKILL.md` in the source. `init` installs the one matching the client you pick, so the instructions are accurate for it, not a generic copy. Only clients with a packaged skill appear in the menu (Claude Code today); if yours is not listed, its skill is not packaged yet, and the flow is documented in the [repository](https://github.com/okedeji/mcpvessel).

Every part of this step is best-effort and never fails an otherwise-ready runtime. If the docs bundle is not published to the registry yet, or the `claude` CLI is not on your `PATH`, `init` prints a note with the URL to register by hand and moves on; the skill install is unaffected.

Non-interactively (a pipe, CI) `init` does this only when you pass `--client`; with no flag it does nothing, so an automated `init` never writes outside `~/.mcpvessel`. Pass `--skip-skill` to suppress the whole step, skill and docs both.

## --recreate: rebuild the VM

`--recreate` tears the runtime down and rebuilds it, which is how you apply a changed machine setting. The VM is sized at creation from `~/.mcpvessel/config.json` (`machine.memory_gib`, `machine.cpus`, `machine.disk_gib`); an existing VM keeps its original size no matter what you edit, so raising `machine.memory_gib` only takes effect after a recreate. With `--recreate`, `init`:

1. Prints `Recreating the runtime...`.
2. **Stops the daemon** first. Recreating the VM out from under a running daemon would orphan every container it holds, so the daemon is shut down gracefully (up to 30 seconds) before the VM goes.
3. **Deletes the VM.** This loses every cached image in it; the next setup pulls and rebuilds them from scratch.
4. Falls back into the normal flow, which provisions a fresh VM with the current config and restarts the daemon.

On Linux there is no VM, so `--recreate` deletes nothing; it just stops the daemon, which the following ensure step brings back up. Effectively a daemon restart.

## Linux is a no-op

On Linux `init` assumes the host's containerd and buildkitd are already running at their default sockets. There is no Lima to fetch, no VM to provision, and the phase UI does not appear. `init` still starts the daemon and checks the in-VM binary, so it is worth running once, but there is no minutes-long first-time cost. A missing containerd or buildkitd is not caught by `init`; it surfaces on the first socket connect during a real command.

## Flags

| Flag | Meaning |
| --- | --- |
| `-v`, `--verbose` | Stream the raw Lima provisioning output instead of the phase UI, for when setup is going wrong and you need to see what Lima is doing. Only has an effect on macOS during a first-time (or post-recreate) VM provision; when the VM is already up, or on Linux, there is nothing to stream. |
| `--recreate` | Stop the daemon, delete the VM, and provision a fresh one, applying a changed `machine.memory_gib` (and cpus, disk). Deletes every cached image. On Linux, just a daemon restart. |
| `--client` | Set up this MCP client without prompting (for example `claude-code`): install the skill and register the caged docs server. Lets a scripted `init` do the client setup non-interactively. |
| `--skip-skill` | Skip the whole client setup (skill and docs both), and do not prompt for it. |

`init` accepts no positional arguments; passing one is an error.

## Examples

```sh
# Do the one-time runtime setup now, with the progress UI.
mcpvessel init

# Same, but stream Lima's raw output because setup is hanging.
mcpvessel init --verbose

# Raised machine.memory_gib in ~/.mcpvessel/config.json; apply it.
mcpvessel init --recreate
```

## Notes

- `init` is optional. Skip it and the identical setup, with the same phase UI, runs the first time a command needs the runtime. `init` only moves that cost to a moment you choose.
- The VM is a single shared instance for this machine, not one VM per agent. Its state is kept under `~/.mcpvessel/lima`, isolated from any other Lima instances you run (Colima, Rancher Desktop, plain Lima).
- Editing machine settings in `config.json` does not resize a VM that already exists. Only `--recreate` applies the new size, and it costs you the image cache.
- On macOS the default VM is 8 GiB RAM, 4 CPUs, and a 60 GiB disk when the config leaves them unset. On Linux `machine.memory_gib` caps the host RAM admitted against; cpus and disk are ignored (there is no VM to size).
- Everything `init` produces lives under `~/.mcpvessel`: the fetched Lima, the VM and its cache, the daemon log, and the daemon socket. Removing that directory (see the uninstall steps) resets the runtime to unprovisioned.

## Uninstall

Everything `init` sets up comes back out in three steps: stop the runtime, remove the binary, delete the state directory.

```sh
mcpvessel daemon stop
brew uninstall --cask mcpvessel   # or delete the binary you installed
rm -rf ~/.mcpvessel
```

Deleting `~/.mcpvessel` removes the macOS VM, cached images and bundles, the daemon log and socket, your signing key, your pinned publisher keys, and your config and secrets. If you might come back, keep the signing key first (`mcpvessel keys export`, before removing the binary); a re-install otherwise generates a new one, and anyone pinned to your old key sees a mismatch on your next push. With `VESSEL_HOME` set, that directory is the one to delete instead.

To reset a broken runtime without uninstalling, `mcpvessel init --recreate` rebuilds the VM and keeps your bundles, secrets, and config.

## See also

- [import](import.md): the first command that actually needs the runtime `init` prepares.
- [ARCHITECTURE.md](ARCHITECTURE.md): what the VM, the daemon, and the broker containers are for.
- [Requirements](../README.md#requirements): the one-time setup cost, in the same words.
- [troubleshooting](troubleshooting.md): the failures `init` and the daemon can produce, with fixes.
