# egress

Approve or reject an outbound host a caged server is trying to reach. A run is deny-default: a server reaches only the hosts you have allowed, and the first time it reaches a new one the connection is held while mcpvessel asks you. `egress allow` releases a held host and remembers it; `egress deny` rejects one and forgets it; `egress ls` shows what is currently held. This is how you let a server talk to the internet without knowing its hosts in advance and without ever opening it wide.

```
mcpvessel egress allow TARGET HOST [--once]
mcpvessel egress deny  TARGET HOST
mcpvessel egress ls
```

`TARGET` is the tag you ran (`@org/name:version`) or a run id from `mcpvessel ps`. `HOST` is the hostname the cage was held on, shown in the hold notification.

## The deny-default model

Every caged server starts with no outbound network. It reaches a host only if that host is in its allow-set, and the allow-set is built from four sources, in order of how permanent they are:

1. **The bundle's own `EGRESS allow:`** directive, the author's baseline, baked into the bundle.
2. **The operator's `--egress`** on `run`/`serve`, allowed for that one run.
3. **The operator's config**, `mcpvessel config egress`, allowed for every run of a tag.
4. **An interactive approval**, `mcpvessel egress allow`, which allows a held host now and writes it into the config so the next run does not ask again.

Anything not in that set is not refused outright. The proxy **holds** the connection and asks you. That is the difference from a plain firewall: a host you did not anticipate does not fail the call, it pauses it, and you decide.

## What a hold looks like

When a server reaches an unapproved host, three things happen at once, so whoever is positioned to answer can:

- **At a terminal** (`run`/`call` in the foreground), you get an inline prompt: `egress pending: api.github.com. Allow this host? [y/N]`. Answer `y` and the call continues.
- **In the run's output and the event feed**, a line names the host and the exact command to approve it: `mcpvessel egress allow <run> <host>`. `mcpvessel events` carries the same, so a watcher or a script can react.
- **`mcpvessel egress ls`** lists every held host across runs, each with its approve command.

You approve from wherever is convenient: type `y` at the prompt, or run `egress allow` from another shell. Either releases the same held connection.

### The one caveat worth knowing

A held connection counts against the *server's own* network timeout. Most servers give a connection only a few seconds before they give up, which is fine when you answer an inline prompt in the moment, but means an approval you take a minute to make can arrive after the server already errored. That is not a problem in practice: the approval is still remembered, so the next run of that tag reaches the host with no hold at all. First call slow to approve, retry instant.

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

## Arguments and flags

| Argument | Meaning |
| --- | --- |
| `TARGET` | The tag (`@org/name:version`) or run id whose held host to decide. A tag matches every live run of it. |
| `HOST` | The hostname to allow or deny, as shown in the hold notification. |

| Flag (on `allow`) | Meaning |
| --- | --- |
| `--once` | Release the host for the live run only; do not remember it in config. |

## Notes

- `egress ls` reads live holds from the daemon; a host drops off it the moment it is approved, denied, or the run ends.
- Approving a tag with several live runs releases the host on all of them.
- The hold is bounded: an unanswered hold fails closed after a few minutes rather than pinning the cage forever.
- A malicious server phoning home shows up as a held host you did not expect. Denying it (or just not approving) keeps the connection, and any secret the server holds, from ever leaving.
- Nothing here relaxes the rest of the cage. Egress is one wall; the filesystem, the secrets, and the sibling isolation are unaffected by an approval.

## See also

- [config](config.md): `config egress` sets persistent allow-lists directly; `config secrets` binds the keys a server needs.
- [run](run.md), [call](call.md), [serve](serve.md): the commands whose held hosts you approve here.
- [ps](ps.md): the run ids `egress allow` accepts.
- [events](events.md): the `egress.pending` and `egress.approved` feed.
- [VESSELFILE.md](VESSELFILE.md): the `EGRESS allow:` and `EGRESS deny-default` directives, the author's baseline.
- [ARCHITECTURE.md](ARCHITECTURE.md): the egress proxy that holds and enforces, and why it is the only way out.
