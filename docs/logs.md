# logs

Show a run's log, live or finished. `logs` reads the run's durable log through the daemon, so it works while the run is up, after it has ended, and after a daemon restart. With `-f` it tails a live run, streaming new output until the run ends.

```
mcpvessel logs RUN [-f]
```

`RUN` is the run id `mcpvessel ps` lists. It takes exactly one, and the command needs a running daemon to read through.

## What a run log holds

A run log is the agent's own output, captured verbatim. When the daemon boots a run it opens a durable log file and hands it to the runtime, which tees the agent container's stderr into it as the agent runs. That is what you see: whatever the caged server or reasoning harness wrote to stderr, boot diagnostics, tracebacks, its own logging.

The cage's egress proxy writes into the same stream. Every time the proxy blocks an outbound host it emits an `egress denied: <host> (agent ...)` line, so a run that tried to reach a host it was not allowed shows the denial inline with the agent's output. The daemon also scans those lines as they pass through, recording the blocked hosts per run so a served tool error can name them (`the cage blocked this server from reaching <host>; allow it with 'mcpvessel run/serve --egress <host>'`). `logs` is where you read the raw denials themselves.

The log is a file on disk at `~/.mcpvessel/logs/<run-id>.log`, opened append-only with `0600` permissions. Because it is a file and not an in-memory buffer, it outlives both the run and the daemon: the history store keeps only metadata, the log keeps the bytes. That is why `logs` can print a finished run's output and why a daemon that restarted mid-run still serves the log it wrote earlier.

## Reading to date, or following

Without `-f`, `logs` reads the log file to its current end and returns. You get everything written so far, whether the run is still up or long gone.

With `-f` (`--follow`), the daemon tails the file. It streams what is there, and at end of file it checks whether the run is still in its live registry. While the run is live it rechecks every 200ms and streams whatever new bytes appear. When the run leaves the live set (it ended) the daemon drains once more so nothing written between the last read and the run's exit is lost, then closes the stream and `logs` returns. Following a run that is already finished simply drains it and returns, same as reading to date. Interrupting the command (Ctrl-C, which cancels the request context) stops the tail at once.

Output goes to stdout as plain text, streamed and flushed as it arrives, so you can pipe it (`mcpvessel logs -f <id> | grep denied`) or redirect it to a file.

## Which runs have a log

Any run the daemon boots gets a durable log: a one-shot `run`, and each per-session instance a `serve` front door spins up behind a URL. Two kinds of entry do not have one:

- **A serve front door itself.** The `serving` row `ps` shows for a served agent is the front door, not a booted instance. It has no log file of its own. The per-session instances behind it each do, under their own run ids.
- **A boot that failed before it got a run id.** If a run never reached the point where the daemon opens its log, there is nothing on disk.

Asking for either, or for a run id that never existed, is a 404: `no logs for run <id>`.

## Flags

| Flag | Meaning |
| --- | --- |
| `-f`, `--follow` | Tail a live run, streaming new output until the run ends, then return. Without it, read the log to its current end and return. On an already-finished run it behaves like a plain read. |

## Examples

```sh
# Read a run's full log, start to finish.
mcpvessel logs researcher-7a1c4f2e9d3b

# Follow a live run, streaming until it ends.
mcpvessel logs -f researcher-7a1c4f2e9d3b

# Pull just the egress denials out of a finished run.
mcpvessel logs researcher-7a1c4f2e9d3b | grep 'egress denied'

# Watch a served instance live and save what you see.
mcpvessel logs -f me-github-3f9a1c22-b17e | tee github.log
```

## Notes

- `logs` does not start the daemon. If none is running the command fails with `is the daemon running? start it with 'mcpvessel daemon'`, and the same hint is appended to every other error, including the 404 for an unknown run.
- The run id comes from `ps`, which lists only currently live runs. A finished run's log is still readable if you have its id from earlier; `logs` reads the file directly and does not need the run to be live.
- Run ids are the agent's friendly name plus a short content hash for a one-shot run (`researcher-7a1c4f2e9d3b`), and the served address plus a session hash and a per-boot suffix for a per-session instance (`me-github-3f9a1c22-b17e`).
- The log is the agent's stderr, not a structured event feed. For a finished reasoning run's LLM steps and cost, that lives in the run trace, not here.
- If the daemon cannot open a run's log file when the run boots it warns and falls back to a no-op sink, so the run proceeds but has no log to read later.

## See also

- [run](run.md), [serve](serve.md): the commands that boot the runs `logs` reads.
- [daemon](daemon.md): the process that opens each run's durable log and serves `logs` over its socket.
- [egress](egress.md): approving the hosts a run is held on, which is what the `egress pending` and `egress allowed` log lines are about.
- [Cage it](../README.md#cage-it): the egress model behind the denials a run log records.
