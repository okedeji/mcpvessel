# ps

List the runs the daemon knows about. `ps` asks the daemon for its run set, overlays the live runs it is holding on top of the durable history it has recorded, and prints one row per run: its id, the agent reference, its status, when it started, and what it cost. The default view keeps live runs on top and trims old history; `-a/--all` shows everything.

```
mcpvessel ps
mcpvessel ps -a
```

`ps` is a read of daemon state. It changes nothing. It needs a daemon running, since the whole answer comes from one call to the control socket.

## What it talks to

`ps` resolves the control socket (`~/.mcpvessel/mcpvessel.sock`, or under `VESSEL_HOME` if you set it), dials it, and issues a single `GET /runs`. That request returns the merged run list; `ps` renders it as a table and exits. There is no local state, no cache, and no filtering: what you see is exactly what the daemon reports at that instant.

If the socket cannot be reached, the call fails and `ps` wraps the error with a hint:

```
... (the daemon is not running; start it with 'mcpvessel init')
```

An unreachable daemon is the common case here (nothing started it, or it was stopped), so the message points straight at the fix rather than leaving you with a bare dial error.

## The table

One header row, then one row per run. With no runs at all there is no table; `ps` prints a single empty-state line instead:

```
No runs yet. Start one with 'mcpvessel run' or 'mcpvessel serve'.
```

| Column | Source | Meaning |
| --- | --- | --- |
| `RUN ID` | `RunInfo.ID` | The run's identifier. For a one-shot or held run it is the runtime run id. For a served agent it is the address the front door exposes. For a per-client served instance it is a derived id (`address-<session>-<suffix>`). |
| `REF` | `RunInfo.Ref` | The agent reference as it was named when the run started: the reference you typed, or the resolved agent name. |
| `STATUS` | `RunInfo.Status` | `serving`, `running`, `stopped`, `succeeded`, `failed`, `over_budget`, or `crashed`. See below. |
| `STARTED` | computed | How long ago the run started, as one coarse unit. |
| `COST` | `RunInfo.CostMicroUSD` | Metered spend for a finished run, blank when nothing was metered. |

### RUN ID

The id depends on how the run entered the daemon. A one-shot run (`run`, `call`, `eval`) carries the runtime's own run id. A served front door is registered under the network address it listens on, so its `RUN ID` is that address. Each per-client instance the front door spawns is itself a run, with an id derived from the address, a hash of the client session, and a unique suffix, so several clients against one served agent show as several rows.

### REF

The reference the run was started under, kept verbatim. When you served or ran by a tag, that tag shows. When the target resolved to a bare content hash (a directory build, say), the daemon falls back to the agent's own name so the row still reads as something recognizable.

### STATUS

Three statuses describe live runs the daemon is currently holding:

- **`serving`** is a serve front door: the HTTP pool that exposes an agent's tools. The front door is not a run in its own right, it is the entry that owns a pool of per-client instances. It stays up until its last run stops.
- **`running`** is a live run in flight: a one-shot run held over stdio, or a single per-client instance a front door has spawned for a connected client.
- **`stopped`** is a per-client served instance that has finished. When a client disconnects or its instance is torn down, that instance closes out as `stopped`.

The rest are terminal statuses read back from history:

- **`succeeded`** and **`failed`** are how a one-shot run ended.
- **`over_budget`** is a `failed` run whose final spend met or exceeded its budget: the daemon escalates the status when it closes the run out, so a budget exhaustion is legible as such rather than a generic failure.
- **`crashed`** is a run whose daemon died under it. A record left in the `running` state at daemon startup is one no live daemon owns anymore, so the next startup reconciles it to `crashed`. You will not see this produced by a healthy daemon; it surfaces a prior daemon that was killed mid-run.

### STARTED

`STARTED` is how long ago the run began, rendered as a single coarse unit: seconds under a minute (`45s`), minutes under an hour (`5m`), hours under a day (`2h`), days beyond that (`3d`). It is truncated, not rounded, so a 90-minute run shows `1h`. Finished runs show their age too, so a row that ended yesterday reads `1d`; a run with no recorded start time shows a dash.

### COST

`COST` is the run's metered spend, formatted as dollars (`$0.0231`), trimmed so a sub-cent spend keeps its precision rather than rounding to zero. It is blank, not `$0.0000`, when nothing was metered. Two things follow from that:

- A live run shows a blank cost. Spend is only read off the gateway when a run finishes, so cost fills in at the end, not during.
- A tool collection, a served front door, or any run that never made a metered LLM call stays blank. Only runs that actually spent show a figure.

## Live plus history overlay

The list the daemon returns is its durable run history with the live run set laid over it. For every run in history, if the daemon is currently holding a live run with the same id, the live entry wins (its in-flight status and start time replace the stored `running` record); otherwise the stored record is used as is. Any live run that has no history record (a `serving` front door is the usual one, since front doors are never recorded) is appended. If the daemon has no history store, the list is just the live set.

The result is ordered live-first: runs still holding containers (`running`, `serving`) print at the top, then finished ones, each group newest first, so what is happening now is never buried under history. By default only the 10 most recently finished runs print; anything older is elided behind a trailer line:

```
... and 4 older; 'mcpvessel ps -a' shows all
```

`-a/--all` lifts the cap and prints the daemon's whole ledger, live and past, in one view.

## Examples

```sh
# Live runs plus the 10 most recent finished ones.
mcpvessel ps

# The full history, nothing elided.
mcpvessel ps -a
```

A daemon serving one agent, with a client connected and a past one-shot run, prints something like:

```
RUN ID                    REF              STATUS     STARTED   COST
@me/github:0.1            @me/github:0.1   serving    8m
@me/github-a1b2c3d4-9f2   @me/github:0.1   running    12s
run-3f9a2c                @me/github:0.1   succeeded  2h        $0.0231
```

An idle daemon with no history prints the empty-state line:

```
No runs yet. Start one with 'mcpvessel run' or 'mcpvessel serve'.
```

No daemon running:

```sh
mcpvessel ps
# Error: contacting the daemon: ... (the daemon is not running; start it with 'mcpvessel init')
```

## Notes

- `-a/--all` toggles the view. The default is live runs plus the 10 most recently finished; `-a` shows every run the daemon reports.
- `--json` emits every run verbatim as a JSON array instead of the table, for an agent driving the CLI. A `serving` row's JSON carries an `endpoint` field, the merged front-door URL a client registers (`http://127.0.0.1:7000/mcp`), so an agent can read it programmatically. Every agent behind one front door shares that one URL (see [serve](serve.md#the-front-door)); the field is present only on `serving` rows.
- With nothing to show, `ps` prints a single empty-state line, not a bare header.
- Live runs never show a cost; cost lands only when a run finishes and its final spend is read off the gateway. A blank `COST` on a live `running` or `serving` row is expected, not a lost figure.
- A `serving` row is the front door, not a session. It carries no cost and never appears in history. The per-client `running` and `stopped` rows under it are the actual sessions, and those are what history records.
- `crashed` only appears when a previous daemon was killed mid-run. A running daemon reconciles those stale records to `crashed` at startup; it never mints one during normal operation.
- The first control-socket call in a process prints a stderr warning if the daemon is running an older build than your CLI, telling you to restart it. This is not specific to `ps`, but you can trip it here.
- `VESSEL_HOME` relocates the socket. Point `ps` and the daemon at the same one or `ps` will not find it.

## See also

- [serve](serve.md), [run](run.md): the commands that create the `serving`, `running`, and finished rows `ps` lists.
- [stop](stop.md): stop a run by the `RUN ID` that `ps` prints.
- [daemon](daemon.md): start, stop, and inspect the daemon `ps` reads from.
- [events](events.md): the live run feed, for watching runs start and end instead of polling `ps`.
- [spend](spend.md), [trace](trace.md): the cost and the per-run detail behind the `COST` column.
- [ARCHITECTURE.md](ARCHITECTURE.md): where the daemon and its run registry sit in the whole.
