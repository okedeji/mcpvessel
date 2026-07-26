# events

Stream a live feed of the daemon's lifecycle events as they happen. `events` connects to the running daemon, prints one line per event (a run starting, a run ending with its final status, a sub-agent cage activating or being evicted, an operator question asked and answered), and stays connected until you interrupt it. In a terminal it prints readable lines; piped or redirected it prints one JSON object per line, so `events` is both a thing to watch and a thing to pipe into a script.

```
mcpvessel events
```

`events` takes no arguments and no flags. It reads the daemon's Unix socket, opens `GET /events`, and forwards whatever the daemon sends. At a terminal it first prints `Listening for daemon events (Ctrl-C to stop)` to stderr, so a quiet feed reads as connected rather than hung. When it cannot reach the daemon it fails with a hint to start one: `the daemon is not running; start it with 'mcpvessel init'`.

## The feed is live and per-subscriber

The daemon holds an in-memory event bus. When you run `events` you subscribe to it, and from that moment you see events as they are published. There is no backfill: events that fired before you connected are not replayed, and nothing on this feed is persisted. To read finished runs after the fact, that is history, not this feed.

Each subscriber gets its own buffered queue (256 events deep). The publisher never blocks on a slow reader: if your queue fills because you fell behind (a paused terminal, a stalled pipe), the daemon drops events for you rather than stall a run's lifecycle. So the feed is best-effort under backpressure. Under normal use you see everything; a reader that can't keep up loses the overflow silently.

The stream stays open for as long as the daemon lives and you stay connected. Interrupt `events` (Ctrl-C) or let its context end and it returns cleanly. If the daemon shuts down or closes the stream, `events` returns without error.

## Output format

`events` picks its format from where its output goes, the same split the rest of mcpvessel's observability output uses.

**To a terminal** it prints one readable line per event:

```
15:04:07  started               run-abc123  @me/oncall:0.1
15:04:09  cage.activated        run-abc123/search
15:04:12  elicitation.asked     run-abc123
15:04:20  elicitation.answered  run-abc123  accept
15:04:31  succeeded             run-abc123  @me/oncall:0.1
```

The columns are the event time (`HH:MM:SS`, local), a label, and a subject, followed by extra detail when the event carries it. The label and subject depend on the event type (see below).

**Piped or redirected** (anything that is not a terminal) it prints newline-delimited JSON, one object per line:

```json
{"time":"2026-07-17T15:04:07.11Z","type":"run.started","run_id":"run-abc123","ref":"@me/oncall:0.1"}
{"time":"2026-07-17T15:04:31.02Z","type":"run.ended","run_id":"run-abc123","ref":"@me/oncall:0.1","status":"succeeded"}
```

This is the raw event as the daemon sends it (the wire format is `application/x-ndjson`). Its fields:

| Field | Meaning |
| --- | --- |
| `time` | When the event fired. |
| `type` | The event type (`run.started`, `run.ended`, `egress.pending`, `egress.approved`, `egress.denied`, `egress.inspect`, `egress.preview`, `cage.activated`, `cage.evicted`, `elicitation.asked`, `elicitation.answered`). |
| `run_id` | The run the event belongs to. |
| `ref` | The bundle reference the run is executing. Present on run and runtime events. |
| `target` | The sub-agent node the event concerns, or the host on an egress event. Empty when the event is run-wide. |
| `status` | The run's terminal status. Present on `run.ended`. |
| `detail` | Extra context: an error message on a failed `run.ended`, the operator's action on `elicitation.answered`. |

Every field except `time`, `type`, and `run_id` is omitted when empty, so a given line carries only what applies.

## Event types

Two kinds of event reach the feed. The daemon emits run lifecycle events (`run.started`, `run.ended`) and egress decisions (`egress.pending`, `egress.approved`, `egress.denied`, `egress.inspect`, `egress.preview`) directly. The runtime emits in-process events for a run's sub-agent tree (`cage.activated`, `cage.evicted`, `elicitation.*`), which the daemon forwards onto the same feed. Per-LLM-call and per-sub-agent-call telemetry does not appear here; that reaches the daemon over a separate channel and surfaces in a run's trace.

### run.started

A run began. Emitted when the daemon boots a held run (`run`, an interactive session) and when a served bundle accepts a new per-client instance (each such instance is a run). The front-door pool that fronts a served bundle is not itself a run, so it stays off the feed.

In a terminal the label is `started`, the subject is the run id, and the run's `ref` is appended.

### run.ended

A run finished. Emitted by the daemon when it closes a run out, after it has read the run's final spend and written its history record. The `status` field carries the terminal status:

- `succeeded`: the run's call returned without error.
- `failed`: the call errored.
- `over_budget`: a failed call whose spend reached or passed its budget. The daemon escalates `failed` to this when the telemetry shows the budget was exhausted.
- `stopped`: the run was stopped (an interactive session ended, a served instance disconnected).

On a failed run, `detail` carries the error message. In a terminal the label is the status itself (not `run.ended`), the subject is the run id, and the `ref` and any `detail` are appended.

(`crashed` is a status history assigns to runs that were still running when a daemon died, reconciled on the next daemon start. It is not published on this live feed.)

### egress.pending, egress.approved, egress.denied

A caged server's outbound connection reached the egress proxy's decision path. `target` is the host in every case. `egress.pending` fires when a server reaches a host not yet in its allow-set and the proxy holds (or, under `serve`, fails-fast) the connection; `detail` carries the exact `mcpvessel egress allow` command. `egress.approved` fires when a held host is allowed and the connection proceeds; `egress.denied` when it is rejected or the hold lapses. In a terminal the label is the type and the subject is `run_id/target`. These are the live view of the deny-default flow that [egress](egress.md) covers.

### egress.inspect

Under `--egress-inspect` only, one per inspected request to an approved host. `target` is the host; `detail` is the secret-safe summary the proxy emits, method, path (query stripped), byte counts, and status, never a body or query value. In a terminal the label is `egress.inspect` and the subject is `run_id/target`. The full request and response (headers and bodies) are not on this feed; they are captured into the `.replay` artifact when the run is recorded. See [egress](egress.md#seeing-what-a-server-sends---egress-inspect).

### egress.preview

Under `--egress-inspect` only, when a cage's request to a *not-yet-approved* host is captured and held for the operator's decision. `target` is the host; `detail` is the same secret-safe summary as `egress.inspect`, never the body. It is the signal that a request is waiting to be read before approval: on a `run`/`call` the daemon shows it inline at the prompt, and on `serve` it is read with `mcpvessel egress preview <run> <host>`. The full request (with body) is pulled on demand, never on this feed. See [egress](egress.md#see-the-request-before-you-approve-the-host).

### cage.activated

A sub-agent cage booted. In a `USES` tree, a sub-agent is not started until it is first needed; when the runtime activates one, this fires. `target` names the node that came up. In a terminal the label is `cage.activated` and the subject is `run_id/target`.

### cage.evicted

A sub-agent cage was torn down to reclaim a slot. When a run is at its live-cage cap and needs room for another sub-agent, the runtime evicts the least-recently-used idle cage; this fires for the one it removed. `target` names the evicted node. In a terminal the label is `cage.evicted` and the subject is `run_id/target`. An evicted sub-agent re-activates (another `cage.activated`) the next time it is called.

### elicitation.asked and elicitation.answered

A cage asked the operator a question mid-call, and the operator answered. MCP's elicitation lets a server pause a tool call to ask for input; when a caged sub-agent does this, `elicitation.asked` fires as the question goes out and `elicitation.answered` fires when a response comes back. These are run-wide (no `target`). On `answered`, `detail` carries the operator's action (for example `accept`, `decline`, `cancel`). A question that no one is available to answer, or that times out, fails the call and produces no `answered` event.

## Flags

`events` has no flags and takes no arguments. The output format is chosen automatically from whether output is a terminal.

## Examples

```sh
# Watch the feed live in a terminal.
mcpvessel events

# Follow only the terminal lines mentioning eviction.
mcpvessel events | grep evict

# Pull structured events into jq (the pipe switches output to JSON).
mcpvessel events | jq 'select(.type == "run.ended") | {run_id, status}'

# Record the feed to a file for later inspection.
mcpvessel events > events.ndjson
```

## Notes

- The feed shows only what happens after you connect. Start `events` before the run you want to watch, or you will miss its start.
- Nothing here is stored. To inspect a run after it ends (its cost, its trace, its final status), use the run's history rather than the event feed.
- A reader that falls far enough behind loses events to the 256-deep per-subscriber buffer. `events` will not stall a run to keep a slow watcher in sync.
- The format switch is on the output stream, not a flag. `mcpvessel events | cat` and `mcpvessel events > file` both emit JSON; only a real terminal gets the readable lines. There is no flag to force one or the other.
- If the daemon is not running, `events` errors immediately with the hint to start one. It does not auto-start a daemon.

## See also

- [daemon](daemon.md): the background service whose lifecycle this feed reports, and how to start it.
- [run](run.md), [serve](serve.md): the commands that produce `run.started` and `run.ended` events.
- [Ship it](../README.md#ship-it): serving bundles, whose per-client instances show up on the feed.
