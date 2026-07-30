# audit

Report what each caged server has done to the network, as a durable per-server feed. The daemon captures every egress fact a cage produces (a host it was blocked from, a host held for approval, a granted secret it tried to ship, and under `--egress-inspect` the redacted request behind each attempt) and folds it into a ledger keyed by the server's ref. So `audit` shows what a server did while nothing was watching, not just what a live cage is doing this instant. This is the session-start picture: what your caged servers have been reaching for, held across sessions.

```
mcpvessel audit
mcpvessel audit --json
```

`audit` reads daemon state and the durable ledger. It changes nothing. It needs a daemon running.

## Why it exists

When an agent drives mcpvessel (see [skill](skill.md)), it runs `audit` at the start of a session and relays the result: "since you were last here, notes-server tried to reach exfil.attacker.net twice and tried to ship your Stripe key." The value is recorded behavior over time, grounded in facts the daemon captured, not a live gauge that usually reads zero because the cages that did something interesting were reaped.

## The feed

Per server (keyed by its ref, stable across the ephemeral per-client instances), `audit --json` returns:

| Field | Meaning |
| --- | --- |
| `ref` | The server, e.g. `@you/notes:0.1`. |
| `serving` | Whether a live front door or instance for it is up right now. |
| `secrets` | The secret names config binds to it (names only, never values), added by the CLI from config. |
| `summary` | A rolling summary of what was already surfaced and acked. |
| `events` | The events new since the last ack, each with `kind`, `host`, `count`, timestamps, an optional `detail` (the secret name for a `secret` event), and an optional redacted request `sample`. |
| `cursor` | The watermark to pass back to `audit ack`. |

Event `kind` is one of `blocked` (a host the cage was denied), `held` (a host held for a decision), `approved` (a host that was approved), or `secret` (a granted secret detected leaving, `detail` names it). A `sample` is the redacted request the cage wanted to send, method, URL, headers, and a capped body with granted secrets shown as `«NAME»`, so a reader can judge the content long after the cage was reaped. Raw bytes live only in a `.replay` recording.

## The read-then-ack cycle

The feed is a change stream you consume and check off, like a write-ahead log:

1. **Read** (`audit --json`): each server's `summary` plus the `events` new since last time, and a `cursor`.
2. **Report** the summary merged with the new events.
3. **Ack** (`audit ack`): fold the new events into an updated summary and prune them, so they are not surfaced again. Pipe JSON on stdin:

```sh
echo '{"acks":[{"ref":"@you/notes:0.1","cursor":42,"summary":"rolling summary text"}]}' | mcpvessel audit ack
```

For each entry, the daemon stores your `summary` and drops the events at or below `cursor`. Events that arrived after your read keep their place for next time. So the ledger stays bounded: a compacted summary plus a short tail of unacked events.

The summary is written by whoever consumes the feed (an agent driving mcpvessel writes it the way it compacts context). mcpvessel captures the facts; it does not judge or summarize them.

## What is captured, and what is not

- Egress **facts** (blocked/held/approved hosts, secret detections) are captured for every cage, always.
- A redacted request **sample** is captured for held (new-host) attempts under `--egress-inspect` only; without inspection there is no body to capture, just the host and metadata.
- Granted-secret detection is mcpvessel's, because only mcpvessel knows the secret values it injected. Judging anything else suspicious in a sample (a server's own hardcoded key, the user's data) is the reading agent's job.
- The ledger lives at `egress-ledger.json` under `~/.mcpvessel` (or `VESSEL_HOME`), written owner-readable. It holds redacted samples, so treat it as sensitive, like a recording.

## Examples

```sh
mcpvessel audit
```

```
@me/notes:0.1  (serving now)
  secrets it can see: STRIPE_SECRET_KEY
  new (cursor 3):
    secret exfil.attacker.net (STRIPE_SECRET_KEY) [request captured]
    blocked exfil.attacker.net x2

@me/github:0.1  (seen before)
  summary: reached api.github.com only; approved.
  nothing new since last check.
```

With no caged servers on record:

```
No caged servers on record yet. Serve one with 'mcpvessel serve'.
```

## See also

- [skill](skill.md): the agent-driven path that reads the feed and acks it.
- [egress](egress.md): the deny-default model and the `--egress-inspect` capture the feed is built from.
- [logs](logs.md): the full durable log of a single cage, behind an audit entry.
- [replay](replay.md): the full, unredacted recording when you record a run.
