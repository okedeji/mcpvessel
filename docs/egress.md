# egress

Approve or reject an outbound host a caged server is trying to reach. A run is deny-default: a server reaches only the hosts you have allowed, and the first time it reaches a new one mcpvessel surfaces it for you to decide (holding a foreground call, or failing a served one fast so the client can retry). `egress allow` releases a held host and remembers it; `egress deny` rejects one and forgets it; `egress ls` shows what is currently held. This is how you let a server talk to the internet without knowing its hosts in advance and without ever opening it wide.

```
mcpvessel egress allow TARGET HOST [--once] [--agent NAME | --all]
mcpvessel egress deny  TARGET HOST [--agent NAME]
mcpvessel egress ls
```

`TARGET` is the tag you ran (`@org/name:version`) or a run id from `mcpvessel ps`. `HOST` is the hostname the cage was held on, shown in the hold notification.

## The deny-default model

Every caged server starts with no outbound network. It reaches a host only if that host is in its allow-set, and the allow-set is built from four sources, in order of how permanent they are:

1. **The bundle's own `EGRESS allow:`** directive, the author's baseline, baked into the bundle.
2. **The operator's `--egress`** on `run`/`serve`, allowed for that one run.
3. **The operator's config**, `mcpvessel config egress`, allowed for every run of a tag.
4. **An interactive approval**, `mcpvessel egress allow`, which allows a held host now and writes it into the config so the next run does not ask again.

Anything not in that set is not refused outright: the host is surfaced to you to decide, and how the call behaves while you decide depends on who is driving it.

## What a block looks like

When a server reaches an unapproved host, the proxy always surfaces it the same way, and the pending call behaves according to who can answer:

- **At a terminal** (`run`/`call` in the foreground), the connection is **held** and you get an inline prompt: `egress pending: api.github.com. Allow this host? [y/N]`. Answer `y` and the same held call continues, no retry needed.
- **When served to a client** (`serve` behind Claude or another MCP client), the call **fails fast** instead of hanging: the client cannot answer a prompt, so the tool error tells it the host was blocked and how to allow it, the client relays that to you, you approve out of band, and the client retries. The retry passes.
- **In the run's output and the event feed** (both cases), a line names the host and the exact command to approve it: `mcpvessel egress allow <run> <host>`. `mcpvessel events` carries the same, so a watcher or a script can react.
- **`mcpvessel egress ls`** lists every surfaced host across runs, each with its approve command.

You approve from wherever is convenient: type `y` at a foreground prompt, or run `egress allow` from another shell. Either admits the host on the same live run.

### The one caveat worth knowing

For a foreground `run`/`call`, a held connection counts against the *server's own* network timeout. Most servers give a connection only a few seconds before they give up, which is fine when you answer the inline prompt in the moment, but means an approval you take a minute to make can arrive after the server already errored. That is not a problem in practice: the approval is still remembered, so the next run of that tag reaches the host with no hold at all. First call slow to approve, retry instant. A served call sidesteps this entirely, since it fails fast and relies on the client's retry rather than holding.

## Allowing a host

```sh
# Approve a held host by the tag you ran; remembers it for future runs.
mcpvessel egress allow @me/github:0.1 api.github.com

# Approve by run id (from 'mcpvessel ps'), for this live run only.
mcpvessel egress allow researcher-7a1c4f2e9d3b api.github.com --once

# Reject a host and forget any remembered approval.
mcpvessel egress deny @me/github:0.1 evil.example.com

# See what is currently held, waiting on you.
mcpvessel egress ls
```

`allow` does two things: it releases the connection on every **live** run that matches `TARGET`, and, unless you pass `--once`, it records the host in your config under that tag so it is not asked again. `--once` is for a host you want this run to reach but do not want to trust permanently. A run addressed by id with no registry tag (a local `.agent` or directory) can only be approved `--once`, since there is no tag to remember it under.

An approval is scoped, not broadcast. By default the host is granted to whichever agents actually asked for it, so approving a host a sub-agent was held on never opens it for a sibling that did not request it. Two flags override that default:

- `--agent NAME` pins the grant to one named agent, whether or not it was the one held.
- `--all` grants the host to every agent in the run.

They are mutually exclusive. `deny` takes `--agent NAME` the same way, to reject a host for just one agent. The `egress ls` output ends with a reminder of this model: `Approving grants the host to that agent only; add --all to grant every agent in the run.`

`deny` releases the hold as a rejection (the call sees the host refused) and removes the host from your config if it was remembered, so a mistaken approval is easy to undo.

## Where an approval is remembered

A remembered approval lands in your config, keyed by the tag:

```
~/.mcpvessel/config.json
{
  "egress": {
    "agents": {
      "@me/github:0.1": ["api.github.com"]
    }
  }
}
```

This is the same store `mcpvessel config egress` writes to directly, so `egress allow` and a hand-set `config egress set` are two doors to one place. It is keyed to the exact `@org/name:version`, so a version bump asks again (new code, new judgment). It is operator config, not part of the bundle: it never changes what a teammate pulls. An author who wants to ship a host as a default edits the Vesselfile's `EGRESS allow:` and rebuilds.

## Turning egress off entirely

A server that genuinely needs no network should declare `EGRESS deny-default` in its Vesselfile. That is hard isolation: no egress proxy runs, no host can be held or approved, and an outbound attempt fails immediately rather than pausing. Use it for a pure-compute tool where any outbound connection is a red flag. Absent an `EGRESS` directive, a server is deny-default *with* interactive approval, the model above.

## Seeing what a server sends (`--egress-inspect`)

Egress control decides *which* hosts a server reaches, not *what* it sends to one you allowed. By default the proxy never sees that: it filters on the connection target without terminating TLS, so it holds no payload. Pass `--egress-inspect` to `run`, `call`, `serve`, or `replay record` and that changes for the run: the proxy terminates each cage's HTTPS to an approved host, reads the plaintext to record what was sent, and re-encrypts to the real host, whose certificate it verifies against the system roots (the cage no longer can, so the proxy takes over that check).

It is opt-in and loud. `serve --egress-inspect` prints an `Egress inspection: ON` banner, and each inspected request surfaces live in `mcpvessel events` and `mcpvessel logs` as a one-line summary:

```
egress inspect: gist.github.com (agent notes) POST /gists +query  512B out, 201 128B in
```

The live summary is metadata only, and deliberately so: it names the method, the path (with the query stripped), the byte counts, and the status, but never a body or a query value. That keeps a granted secret out of the log the container runtime persists and out of `mcpvessel logs`, which people paste into bug reports. You do not see the exfiltrated value in the live line, but you see the shape of the theft: a 48KB body leaving a `save_note` call that should send a few hundred bytes is a glaring signal on its own. To read the actual bytes, record the run (below).

### See the request before you approve the host

The most useful moment to know what a server is sending is *before* you approve a new host, because what it is about to send is the actual thing you are deciding on. Under `--egress-inspect`, a request to a not-yet-approved host is captured, not just held: the proxy has the cage's trust, so it terminates the TLS and reads the whole request, then fails that attempt without forwarding a byte. Nothing reaches the host; the captured request waits for you to read and decide.

- On a `run`/`call`, the request is shown inline at the approval prompt: the method, the URL, the headers, and the body (with granted secrets redacted to `«NAME»`), so you read what would have left before answering `[y]/[N]`.
- On `serve` (no operator in the request path), the host is surfaced in `mcpvessel egress ls` with a `preview:` command. `mcpvessel egress preview <run> <host>` prints the full captured request.

Approve the host and the client's next attempt (the MCP client re-drives the tool) reaches it, now inspected and logged. Deny it, or never approve, and nothing ever leaves. Either way you saw the request before a byte went out. This is Little Snitch's prompt with the letter it was about to mail, not just the address on the envelope. It is per-host: you preview the first request to a new host and decide; once the host is approved, later requests to it are inspected and logged, not re-previewed, so a server that sends a benign first request to win approval and exfiltrates later is caught by the ongoing inspection log rather than the preview.

Unlike the live `logs`/`events` summary, the preview shows the body, headers, and query, so you read what the request carries. Any secret you granted the run is redacted first: each granted secret value is replaced by a `«NAME»` marker wherever it appears, so you see *which* secret a server is shipping and *where to*, never the raw value. That redaction is unconditional, it applies to a person at the terminal and to an agent driving mcpvessel alike, so the preview is safe for the agent to read and reason about. The one place the true bytes survive is a `.replay` recording (below), written owner-readable. The redaction matches a granted secret and its common encodings (base64, hex, percent-encoding); a server that encrypts a secret before sending defeats content matching, but then the unexpected host it is reaching is the signal, the same limit a human reader would hit.

Two honest limits:

- **Cert-pinning servers break under inspection.** A server that pins certificates rejects the proxy's leaf, and pinning cannot be detected without first presenting one, so that call fails. The summary names it (`not inspected: cage rejected the inspect certificate`) rather than failing silently. A non-TLS target, or one whose real certificate does not verify, fails the same way (refusing an unverifiable upstream is the point). An HTTP/2-only upstream, which cannot be read as HTTP/1.1, is instead tunneled through uninspected so the call still works, and noted.
- **The live view is a summary; the full detail is in the artifact.** The `logs` and `events` line stays metadata only so it is safe to paste or screen-share. To read exactly what a server sent, headers and body both ways, record the run with `replay record --egress-inspect`: the full captures land in the `.replay` file, verbatim and unredacted, written owner-readable (`0600`). That file is where a granted secret a server shipped is visible as sent, so treat it as sensitive, the same as the LLM prompts and completions a recording already holds. See [replay](replay.md).

## Arguments and flags

| Argument | Meaning |
| --- | --- |
| `TARGET` | The tag (`@org/name:version`) or run id whose held host to decide. A tag matches every live run of it. |
| `HOST` | The hostname to allow or deny, as shown in the hold notification. |

| Flag (on `allow`) | Meaning |
| --- | --- |
| `--once` | Release the host for the live run only; do not remember it in config. |
| `--agent NAME` | Grant the host to just this one named agent. Mutually exclusive with `--all`. |
| `--all` | Grant the host to every agent in the run, not just the one that asked. |

| Flag (on `deny`) | Meaning |
| --- | --- |
| `--agent NAME` | Reject the host for just this one named agent. |

## Approving is the human's decision

Approving an egress host widens the cage, so the decision is always the operator's, never the caged server's and never an agent's own. When an agent drives mcpvessel (see [skill](skill.md)), it may run `egress allow` for you, but only on your say-so: the skill has it put the choice to you (through Claude Code's AskUserQuestion prompt) and run the command you pick, rather than deciding by itself. The cage's deny-default egress is the real backstop: a server reaches nothing new until someone approves it.

If you want a harder guarantee, that these commands cannot run outside an interactive terminal at all, set `VESSEL_STRICT_APPROVAL=1`. Then `egress allow` and the persistent wideners `config egress set/default` and `config secrets set/default` refuse without a TTY, so no agent can perform them. It is off by default. Denying is never gated, since it only tightens the cage.

## Notes

- `egress ls` reads live holds from the daemon; a host drops off it the moment it is approved, denied, or the run ends. `egress ls --json` and `egress preview --json` emit the same data machine-readable, for an agent reading what is held.
- Approving a tag with several live runs releases the host on all of them.
- A foreground hold is bounded: an unanswered hold fails closed after the hold timeout (three minutes by default, set `cages.egress_hold_seconds` in config to change it) rather than pinning the cage forever. A served call does not hold at all; it fails fast and waits for the client to retry.
- A malicious server phoning home shows up as a blocked host you did not expect. Denying it (or just not approving) keeps the connection, and any secret the server holds, from ever leaving.
- Nothing here relaxes the rest of the cage. Egress is one wall; the filesystem, the secrets, and the sibling isolation are unaffected by an approval.

## See also

- [config](config.md): `config egress` sets persistent allow-lists directly; `config secrets` binds the keys a server needs.
- [run](run.md), [call](call.md), [serve](serve.md): the commands whose held hosts you approve here.
- [ps](ps.md): the run ids `egress allow` accepts.
- [events](events.md): the `egress.pending` and `egress.approved` feed.
- [VESSELFILE.md](VESSELFILE.md): the `EGRESS allow:` and `EGRESS deny-default` directives, the author's baseline.
- [ARCHITECTURE.md](ARCHITECTURE.md): the egress proxy that holds and enforces, and why it is the only way out.
