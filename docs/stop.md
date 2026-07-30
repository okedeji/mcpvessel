# stop

Stop one or more running agents and release what they hold. Each argument is a run id that `mcpvessel ps` lists. `stop` talks to the daemon over its control socket, asks it to take the run out of the registry, and tears down the run's containers and networks. It is how you end a run you started with `run`, `call`, or `serve` before it exits on its own.

```
mcpvessel stop RUN...
```

`stop` needs at least one run id (`cobra.MinimumNArgs(1)`); with none it errors before contacting the daemon. It defines no flags. Everything it does is driven by the ids you pass and the state the daemon holds for them.

## What a RUN is

A run id names one live entry in the daemon's registry. `ps` prints it in the `RUN ID` column. The id is the bundle's sanitized name, a short digest of its files, and a random suffix (`researcher-7a1c4f2e9d3b`), so repeated and concurrent runs of the same bundle stay distinct.

Two kinds of entry share the id space, and `stop` ends either one:

- **A held session**, from `run` or `call`: a single agent booted over stdio behind the daemon.
- **A serve entry**, from `serve`: a front door on a listener port owning a pool of per-client agent instances.

## What stopping releases

For each id, the daemon removes the entry from its registry under a lock, so two concurrent stops cannot double-release the same run, then tears it down in a fixed order:

1. **The front door first.** External MCP traffic stops before the agents behind it go away. The run is dropped from its front door; once no run remains behind that door, the door closes and frees its listener port. A run that fronts nothing (a plain `run` or `call`) skips this with no effect.
2. **The final record, for a held session.** Before teardown, while the gateway is still up to answer the spend read, the daemon writes the run's terminal history entry as `stopped` and publishes `run.ended`. A serve entry has no single run lifecycle here, so this step is skipped for it.
3. **The containers and networks.** The held entry is released. A held session tears down its working set: every live or booting cage is stopped, its host slot and network are returned to the pool, and the container is removed. A serve entry releases its whole instance pool, cancelling the pool context and ending every pooled instance. The run's durable log is closed by the runtime's teardown, which owns the handle.

A daemon-wide shutdown does the same teardown per run, so stopping a run by id and shutting the daemon down leave the same clean state. Runs that are not released leak their detached sub-agents and networks to the next reconciliation sweep, which is why `stop` releases rather than just forgets.

## Stopping several runs

Pass more than one id and `stop` works through them in order. It does not stop at the first failure: a run that errors is counted, its `id: error` line is written to stderr, and the loop moves to the next id, so one bad id does not leave the rest running. Each run that stops cleanly prints a `Stopped <id>` line to stdout.

When the loop finishes with any failures, `stop` returns `failed to stop N of M run(s)`, which carries the non-zero exit. The stdout `Stopped` lines are the runs that stopped; the stderr lines are the ones that did not.

## When the daemon is unreachable

If a call cannot reach the daemon at all (no socket, nothing listening), the client returns an `Unreachable` error rather than a per-run failure. `stop` treats this as fatal and aborts the whole batch immediately, because every remaining id would fail the same way and the tally would be noise. The error it returns appends the hint:

```
... (is the daemon running? start it with 'mcpvessel init')
```

This is distinct from an unknown-id error. An unreachable daemon aborts and never reaches the tally; an unknown id is a per-run failure that is counted and stepped past.

## Errors

- **Unknown run id.** If no run in the registry matches the id, the daemon answers `404 no such run <id>`. This surfaces as a per-run failure: it prints to stderr and counts toward the summary, and the rest of the batch still runs.
- **Release failure.** If tearing the run down errors (a container or network that will not release), the daemon answers `500` with the underlying message. Like an unknown id, it counts as a failed run rather than aborting the batch. The run is already out of the registry at this point, since `take` removes it before release runs.
- **No ids.** `stop` with no arguments fails argument validation before any daemon call.

## Arguments and flags

| Argument | Meaning |
| --- | --- |
| `RUN...` | One or more run ids from `mcpvessel ps`. At least one is required. Each is stopped independently; a failure on one does not stop the others. |

`stop` has no flags. It reads the daemon socket path from your home directory and dials it; there is nothing to configure on the command line.

## Examples

```sh
# Stop one run.
mcpvessel stop researcher-7a1c4f2e9d3b

# Stop several at once; each that stops prints its id, each that fails prints to stderr.
mcpvessel stop researcher-7a1c4f2e9d3b oncall-2b8d11c04e7f

# Stop everything ps is showing.
mcpvessel stop $(mcpvessel ps | awk 'NR>1{print $1}')
```

## Notes

- `stop` ends a run; it does not remove the bundle from your store. The bundle stays; only the running instance goes away.
- Stopping a serve entry drops external traffic first, then releases the pool. In-flight requests on that front door end when the door closes.
- A held session's terminal history status is `stopped`, not `crashed` or `failed`. A run you stop reads back in `history` and `replay` as an intentional stop with its final spend recorded.
- The exit code is non-zero if any id failed, so a script can stop a list and detect a bad id without parsing the output. The stdout `Stopped <id>` lines name exactly the runs that stopped.
- Stopping the same id twice is safe: the first stop takes it out of the registry, the second gets `no such run` and counts as a per-run failure.

## See also

- [ps](ps.md): list the run ids `stop` takes, with their references, status, and uptime.
- [run](run.md), [call](call.md), [serve](serve.md): the commands that start the runs `stop` ends.
- [init](init.md): start the daemon `stop` talks to, the fix the unreachable-daemon hint points at.
- [daemon](daemon.md): the control plane that holds the run registry and performs the teardown.
- [ARCHITECTURE.md](ARCHITECTURE.md): the containers and networks a run holds, which stopping releases.
