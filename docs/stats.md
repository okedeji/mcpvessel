# stats

Show live resource usage for every running cage. `stats` asks the daemon for a snapshot of each container's CPU, memory, and process count and prints it as a table, one row per cage. With `-w` it redraws the table in place on a timer so you can watch usage move.

```
mcpvessel stats [-w]
```

`stats` takes no arguments. It reads the numbers the container runtime already reports (nerdctl's own stats) and shows them unchanged, so the values match what the runtime tells you.

## What a cage is

A cage is one container in a run. A single run is usually several: the agent itself, the gateways that front it (the MCP gateway, the LLM gateway, the egress proxy), and any sub-agents it composes. Each is its own container with its own limits, so each gets its own row.

The `CAGE` column is the container's name. The root agent's cage carries the run's id; the auxiliary cages of that run carry it as a prefix with a suffix that names their role (`-gw` for the MCP gateway, `-llm` for the LLM gateway, `-egress-proxy` for the egress proxy, and a per-sub-agent suffix for each sub-agent). Rows from the same run therefore share a prefix, which is how you tell one run's cages from another's.

## The table

Four columns, printed with aligned tab stops:

| Column | What it is |
| --- | --- |
| `CAGE` | The container name (see [What a cage is](#what-a-cage-is)). |
| `CPU` | The runtime's CPU percent for that cage. |
| `MEM` | The runtime's memory reading, usually `used / limit`. |
| `PIDS` | The number of processes running in the cage. |

The values are the runtime's own already-formatted strings, passed through as-is. A field the runtime leaves blank prints as `-` rather than an empty cell, so a missing reading is visible instead of silent.

When the runtime is up but nothing is running, there is no table; `stats` prints a single empty-state line instead: `No live cages. Cages appear here only while a run or a served instance is booted.` That is a normal state, not an error.

## Where the numbers come from

`stats` does not read the runtime itself. It calls the daemon's `/stats` endpoint over the local control socket (`~/.mcpvessel/mcpvessel.sock`, or under `VESSEL_HOME` if you set it). The daemon runs a one-shot `nerdctl stats` inside the runtime (a single non-streaming snapshot, names untruncated), parses the per-container JSON, and returns it. So the daemon must be running, and behind it the runtime (on macOS, the Linux VM) must be up.

Each call is a fresh point-in-time snapshot. A single snapshot means the CPU figure is one sample, not an average over time, so read it as a momentary reading.

## Watching

Default `stats` prints one snapshot and exits. `-w` / `--watch` turns it into a live view: it re-queries `/stats` every 2 seconds, clears the terminal and redraws the table in place, and keeps going until you interrupt it (Ctrl-C). Because it clears the screen with a terminal control sequence, watch mode is meant for an interactive terminal, not a pipe or a log.

If a refresh fails mid-watch (for instance the daemon goes away), watch mode stops and reports the error rather than spinning on a dead socket.

## Flags

| Flag | Meaning |
| --- | --- |
| `-w`, `--watch` | Redraw the table in place every 2 seconds until interrupted. Off by default, which prints one snapshot and exits. |

`stats` accepts no positional arguments.

## Examples

```sh
# One snapshot of every running cage.
mcpvessel stats

# Watch usage live; Ctrl-C to stop.
mcpvessel stats -w

# Start a run, then watch its cages settle.
mcpvessel serve @me/github:0.1 &
mcpvessel stats -w
```

## Notes

- The daemon must be running. If it is not reachable, `stats` fails and tells you to start it with `mcpvessel init`; in watch mode the same failure ends the watch.
- The runtime behind the daemon must be up. If the daemon can reach nerdctl but the runtime is not up to report (no VM, for example), `/stats` answers `stats unavailable (is the runtime up?)`.
- The `No live cages.` line means the runtime is up and nothing is booted. It is not an error.
- The numbers are nerdctl's, unmodified. mcpvessel does not compute or rescale them, so they read exactly as nerdctl's own `stats` would.

## See also

- [daemon](daemon.md): the background process that serves `/stats` and manages the runtime `stats` reads from.
- [run](run.md), [serve](serve.md): what starts the cages `stats` shows.
- [tree](tree.md): the shape of a run (agent, gateways, sub-agents), the same cages `stats` lists as rows.
- [ARCHITECTURE.md](ARCHITECTURE.md): where the daemon and the runtime sit.
