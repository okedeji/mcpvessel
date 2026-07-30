# daemon

Run the long-lived host process that owns every running agent and answers the CLI. The daemon listens on a Unix socket under `~/.mcpvessel` and holds each run over the stdio of a live container subprocess, so `ps`, `logs`, `stop`, `serve`, `run`, and `call` all reach the same agents through it. It runs in the foreground and shuts down cleanly on SIGINT or SIGTERM.

```
mcpvessel daemon
mcpvessel daemon stop
```

You rarely type `mcpvessel daemon` yourself. Any command that needs the daemon spawns one on demand (see [How it gets started for you](#how-it-gets-started-for-you)), and in production a process manager owns it. Run it by hand when you want the daemon in the foreground of a terminal, watching its output live.

## What the daemon owns

The daemon is mcpvessel's control plane. It runs on the host next to the container runtime, not inside a cage, and it is the single process that holds live state:

- **The run registry.** Every held run, whether a one-shot held over a container's stdio or a serve pool of per-client instances, lives in the daemon's memory. Kill the daemon and those handles go with it.
- **Two listeners.** The control plane on the Unix socket answers the CLI. Each `serve` opens a separate MCP-over-HTTP front door on TCP, one per `serve` invocation, bound to the address you gave it. The front doors carry external MCP traffic; the socket carries operator commands.
- **Run history.** It writes each run's opening and terminal record to the durable history store, so `ps` and `trace` can read back runs that have already ended.

Because all of this is in one process, stopping it is not just killing a server. It has containers, per-run networks, and broker sidecars to release first. That is what [`daemon stop`](#daemon-stop) and a SIGTERM both do.

## Where you run it

The daemon runs where the container runtime runs.

- **On Linux** the host's own containerd and buildkitd are used directly, so the daemon runs on this host. In production you start it under systemd. For local development you can start it in a terminal with `mcpvessel daemon`, or just let the first command that needs it spawn one.
- **On macOS** the daemon still runs on the host, same as Linux. What moves into the small Linux VM the runtime provisions is the containers and their runtime (containerd, buildkitd): the daemon drives them from outside, through the VM's shell. So `mcpvessel daemon` behaves identically on both platforms, and the auto-spawn described below is how it usually starts on either.

The listening line it prints on start names the socket it bound:

```
mcpvessel daemon listening on /Users/you/.mcpvessel/mcpvessel.sock
```

`VESSEL_HOME`, when set, replaces `~/.mcpvessel` as the state root, which moves the socket and every other path with it.

## Startup: what a fresh daemon does

Before it accepts a single request, a starting daemon reconciles the mess a previous one may have left, then binds:

1. **Checks the socket path length.** A Unix socket path has a hard OS cap (104 bytes on macOS, 108 on Linux; mcpvessel enforces the conservative 104). A path at or over the cap is rejected up front with the length, the limit, and the fix (point `VESSEL_HOME` at a shorter directory), instead of the kernel's opaque "invalid argument". This only bites when `VESSEL_HOME` points somewhere deep.
2. **Refuses a second daemon.** If something already answers on the socket, it stops with `a daemon is already listening on <socket>`. One daemon per socket.
3. **Clears a stale socket.** If nothing answered but a socket file is still there, it is a crash leftover that would block the bind, so it is removed. Safe, because no live daemon owns it.
4. **Sweeps orphaned containers and networks.** Owning the socket means any daemon-labeled containers or networks belong to a crashed predecessor, so they are torn down before new runs start. Best-effort: a sweep error is logged, not fatal.
5. **Reconciles crashed runs.** Any run the history still marks `running` had its daemon die under it. Those are reconciled to `crashed`, and the count is printed (`reconciled N crashed run(s) from a previous daemon`). If the history store will not open, the daemon logs a warning and serves without history: runs still work, but `ps` and `trace` see less.
6. **Starts the metrics endpoint** if one is configured. When `telemetry` sets a metrics address, a Prometheus scrape endpoint binds there. Best-effort: a listener that will not bind warns and the daemon serves runs without it.
7. **Binds the control plane** on the socket and starts answering.

## How it gets started for you

You mostly never invoke `mcpvessel daemon` directly. Commands that need a daemon call an internal `Ensure` step that spawns one if none is listening:

- It pings the socket (a `/version` call with a 500 ms timeout). If a daemon answers, it uses it.
- If nothing answers, it spawns the daemon detached: the same binary, running the `daemon` subcommand, put in its own session so a Ctrl-C on your terminal does not kill it, and reparented so it outlives the command that launched it. Its stdout and stderr are appended to `~/.mcpvessel/daemon.log`.
- It then polls up to 5 seconds for the new daemon to answer. If it does not come up in time, the command fails with `daemon did not come up within 5s; check ~/.mcpvessel/daemon.log`. Binding is near-instant, so this timeout only outlasts process startup.

The spawned daemon inherits the launching command's environment, so its version and `VESSEL_HOME` (and therefore its socket and store paths) match the CLI that started it. `mcpvessel init` takes this start latency up front on purpose, so the first real run is not the one that pays for it.

## The identity handshake

A daemon spawned before you rebuild or upgrade mcpvessel keeps running the old orchestration code, and it looks perfectly healthy while doing it. The only way that mismatch becomes visible is a version and binary handshake on every control-plane response.

Every response the daemon returns carries three headers stamped by `stampIdentity`:

- `Mcpvessel-Version`: the daemon's build version.
- `Mcpvessel-Binary`: the path of the executable the daemon is running.
- `Mcpvessel-Binary-Mtime`: that file's modification time, captured once at the daemon's first request (effectively its start).

The CLI checks them once per process (`checkStale`):

- If the header version differs from the CLI's own version, the daemon is stale.
- If the versions match, the CLI stats the daemon's binary on disk and compares its mtime to the stamped one. A rebuild changes the file's mtime but not the running daemon's stamp, and an upgrade may unlink the file entirely. A changed mtime, or a binary now missing, means stale.
- If the headers are absent (an older daemon that never stamped them), the check is skipped rather than firing a false positive.

When it detects a stale daemon it prints one warning and moves on:

```
warning: the daemon is running a stale mcpvessel build; restart it with 'mcpvessel daemon stop && mcpvessel init'
```

The warning does not block the command. It only tells you the running daemon is not the build you think it is, so you can restart it.

## daemon stop

```
mcpvessel daemon stop
```

Ask the running daemon to shut down cleanly, releasing every agent it holds before it exits so nothing is orphaned.

This is the supported alternative to killing the process. A SIGKILL would leave a run's containers and its per-run network behind for the next daemon's startup sweep to clean up. `daemon stop` instead:

1. Dials the socket and checks whether a daemon answers. If none does, it prints `No daemon is running.` and exits successfully. Stopping nothing is not an error.
2. Sends the shutdown request. The daemon acks immediately, then triggers its own shutdown, so the request finishes before the process goes down.
3. On the way down the daemon closes its front doors first, stopping external MCP traffic before the runs behind them are released, then releases every held run. A run held over stdio is recorded as `stopped` (a clean stop, not a crash) before its session, container, and network are torn down; a serve pool releases all its per-client instances. Same path a SIGTERM from systemd or launchd takes.
4. `daemon stop` confirms the shutdown by polling the socket until it stops answering, up to 30 seconds, not by trusting the ack (the ack can race the socket closing). If the daemon is still answering after 30 seconds it fails with `daemon did not stop within 30s; check ~/.mcpvessel/daemon.log`. The window is generous because a graceful stop releases held runs first.

On success it prints `Stopped the daemon`.

In production the process manager owns the daemon and stops it with SIGTERM, which runs this exact clean shutdown. Reach for `daemon stop` in local development, where you started the daemon yourself.

## Flags

`mcpvessel daemon` and `mcpvessel daemon stop` take no flags and no arguments. What the daemon reads, it reads from the environment and config file, not the command line:

| Input | Effect |
| --- | --- |
| `VESSEL_HOME` | Moves the state root off `~/.mcpvessel`. The socket, run history, and `daemon.log` all move with it. Inherited by an auto-spawned daemon from the command that spawned it, so the two agree on paths. |
| `telemetry` metrics address (config) | When set, the daemon exposes a Prometheus scrape endpoint at that address on start. Best-effort. |
| `serve` limits (config) | The daemon reads the max-clients and client-idle settings when it opens a serve front door's instance pool. |

## Examples

```sh
# Run the daemon in the foreground and watch its output live (Linux, local dev).
mcpvessel daemon

# Stop it cleanly, releasing every agent, container, and network it holds.
mcpvessel daemon stop

# Restart after a rebuild, the fix the stale-build warning points you to.
mcpvessel daemon stop && mcpvessel init

# Point the daemon at an alternate state root (socket, history, and log move too).
VESSEL_HOME=/tmp/mv mcpvessel daemon
```

## Notes

- `mcpvessel stop` and `mcpvessel daemon stop` are different commands. `stop` releases one running agent (a run) through the daemon. `daemon stop` shuts down the whole daemon and every run it holds.
- The listening line goes to stderr. When the daemon is spawned for you, that line and everything else it writes land in `~/.mcpvessel/daemon.log`, which the startup and shutdown timeout errors both point you at.
- A daemon that dies uncrashed does not clean up after itself. The next daemon does, on startup: it sweeps orphaned containers and networks and reconciles any run still marked running to crashed. That recovery is automatic, but it is why `daemon stop` (or a SIGTERM) is worth using over a hard kill.
- The daemon holds runs in memory. Restarting it does not resume them: held runs are released on the way down, and history remembers them as stopped or crashed, not as things to bring back.
- Only one daemon can own a socket. If `mcpvessel daemon` reports a daemon is already listening, one is already up; use it, or `daemon stop` first.

## See also

- [init](init.md): prepares the runtime and takes the daemon's start latency up front.
- [serve](serve.md): opens a front door on the daemon that external MCP clients connect to.
- [ps](ps.md), [logs](logs.md), [stop](stop.md): the run-facing commands that all talk to the daemon over its socket.
- [ARCHITECTURE.md](ARCHITECTURE.md): the broker containers and per-run networks the daemon supervises.
- [Uninstall](../README.md#uninstall): `daemon stop` as the first step in tearing mcpvessel down.
