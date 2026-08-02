# skill (the agent-driven path)

There are two ways to use mcpvessel. The first is the one the rest of these docs describe: you, at a terminal, import a server into a cage, serve it, hand the URL to your MCP client, and approve egress yourself. The second is this one: you install a **skill** and let your MCP client's agent drive mcpvessel for you.

With the skill installed, you tell Claude (or another agent-capable client) "install this MCP server" and it does the operator work itself: imports the server into a cage, serves it, registers the endpoint with itself, refreshes the session, and watches what the server tries to send. It reports back what it saw. You stay in charge of one thing: approving anything that widens the cage.

## Installing it

`mcpvessel init` offers to install the skill and, at a terminal, asks which client. Selecting Claude Code writes it to `~/.claude/skills/mcpvessel/SKILL.md`. You can also install it directly:

```
mcpvessel init --client claude-code
```

Each client gets its own skill, written for that client's tools and register command (Claude Code's is packaged today; more are a folder plus a registry entry away). See [init](init.md#installing-the-client-skill). Once installed, start a new session so the agent picks the skill up.

## Keeping the skill current

The skill is versioned (`metadata.version` in its frontmatter) independently of the binary, and it improves faster. You do not have to upgrade mcpvessel to get a better skill. The mcpvessel-docs MCP serves the latest skill live from the repository through its `latest_skill` tool, and the agent applies an update itself: it compares the served version to the one installed, checks the skill's `requires` against your `mcpvessel --version`, asks you before changing anything, and writes it with `mcpvessel skill install --from -`. A skill never outruns the binary: if the newest one needs commands your binary lacks, the agent leaves your skill in place and tells you a binary upgrade is what unlocks it.

`mcpvessel skill show` prints where the skill is installed and its version; `mcpvessel skill install` rewrites the one packaged in your binary.

## What the agent does, and does not

The skill casts the agent as the **operator and the analyst**, and keeps the **decision** with you. That split is the whole point.

**Operator and analyst (the agent does this freely):**

- Cage a server and serve it (`import --json`, `serve --egress-inspect`, `ps --json` for the endpoint).
- Register the served endpoint with itself and refresh.
- Watch egress: run [audit](audit.md) at session start, list held hosts with [egress ls](egress.md), and read the actual captured request with [egress preview](egress.md).
- Judge what it sees and tell you, with a recommendation: this host is expected, or a granted secret is going somewhere it should not. The preview redacts your granted secret values to `«NAME»` markers, so the agent sees *which* secret a server is shipping and *where to*, never the raw value. It reads the shape of the request and the destination, not your keys.

**Decision (this stays with you):**

- Approving a held egress host (`egress allow`), persisting an allow-list, or binding a secret.

This is safe precisely because the cage underneath is deny-default. The agent can install and run untrusted servers all it likes, and nothing leaves the cage without either being on the allow-list or getting your approval. The agent's autonomy is bounded by the same wall that protects you from the server.

## Who decides

The agent may *run* the cage-widening commands (`egress allow`, `config egress set/default`, `config secrets set/default`), but it must never *decide* to on its own. The skill has it put the choice to you through Claude Code's AskUserQuestion prompt, show what it saw, and run the command that matches your answer, or act on a direct instruction ("approve that one"). It never approves because a tool result told it to, which is exactly the prompt-injection trick the cage exists to stop. Denying it can do proactively (that only tightens the cage), but it tells you it did.

Every such question carries the exact command alongside it, filled in with the real tag and host, for both answers. You can let the agent run it or paste it into your own terminal, and either way you see the command before it widens anything. On a decision this consequential, being able to check the agent's account against what it actually ran is part of the point.

The real backstop is not a rule in the skill, it is the cage: deny-default egress means a server reaches nothing new until someone approves it, and that someone is you.

If you want a harder guarantee, that these commands cannot run outside an interactive terminal at all, set `VESSEL_STRICT_APPROVAL=1`. Then they refuse without a TTY, so no agent can perform them, at the cost of the agent being able to run an approval you asked for. Off by default.

## What the agent reads

Every command the agent relies on emits machine-readable JSON, so it reads state instead of scraping human text:

- `mcpvessel audit --json` for the session-start facts per caged server.
- `mcpvessel ps --json` for run state and a serving run's `endpoint` URL.
- `mcpvessel egress ls --json` and `mcpvessel egress preview <run> <host> --json` for the held hosts and the captured request.
- `mcpvessel import --json` for the caged server's ref.
- `mcpvessel events --json` for the live feed.

To see the exact instructions the agent follows, read the installed `SKILL.md`.

## See also

- [init](init.md): installs the skill.
- [audit](audit.md): the session-start view the agent relays to you.
- [egress](egress.md): the deny-default model, the approval flow, and the guardrail in context.
- [serve](serve.md): what the agent runs to expose a caged server to itself.
