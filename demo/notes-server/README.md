# The demo notes server

A deliberately malicious MCP server, used to record the demo GIF in the project
[README](../../README.md). It exposes one honest-looking tool, `save_note`, that
[quietly tries to POST your Stripe key](server.py) to `exfil.attacker.net` on every
call. Caged by mcpvessel, that outbound connection never opens: the tool still
returns a clean success, and the attempt shows up on your side alone.

It is a fixture, not an example to copy. The exfil host does not resolve and is
denied whether or not it did, so the recording is fully offline.

## Rebuild and try it yourself

```sh
# A throwaway value for the server to try (and fail) to steal.
printf 'sk_live_fake_demo_key' | mcpvessel secrets set STRIPE_SECRET_KEY

# Build the caged server from this directory.
mcpvessel build ./demo/notes-server -t @me/notes:0.1

# Serve it with the secret and NO egress grant.
mcpvessel serve @me/notes:0.1 --listen 127.0.0.1:7799 --secret STRIPE_SECRET_KEY
```

Then watch mcpvessel's live audit feed in one terminal while, in another, you
point Claude Code at the caged server and ask it to save a perfectly mundane
note, exactly the way you would use any MCP server:

```sh
# Terminal 1: watch every egress decision as it happens.
mcpvessel events
```

```sh
# Terminal 2: wire Claude to the one front-door URL that fronts every tool,
# then ask it to save an ordinary note. It calls the tool and reports success.
claude mcp add -t http notes http://127.0.0.1:7799/mcp
claude -p "Save a note: pick up groceries after work" \
  --allowedTools "mcp__notes__me-notes_save_note"
# Note saved.
```

Claude sees a tool that just worked. Terminal 1 shows what it tried to hide:

```
{"type":"egress.denied","target":"exfil.attacker.net", ...}
```

You asked it to save groceries; it tried to ship your `STRIPE_SECRET_KEY` to an
attacker. The connection never opened, and the theft shows up only on your side,
in the audit feed the caged server cannot see or silence.

