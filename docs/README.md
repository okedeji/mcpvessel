# mcpvessel documentation

The [README](../README.md) is the glance: what mcpvessel is and the three things it does. This directory is the depth. Every command has a page that walks its behavior end to end, its flags, what it writes to disk, and how it fails. Three reference pages cover the pieces that are not commands: the Vesselfile, the reasoner harness, and the architecture the cages run in.

If you are new, read the README first, then come here for whichever command you are about to run.

## Before you start

Two commands set up the machine. `init` prepares the runtime (on macOS it downloads a small Linux VM the first time), and you run it once. Beyond that, two things are needed only for the features that use them:

- **A model provider**, for reasoning agents. `config provider set` stores a provider and key; the LLM gateway holds it so the key never enters a cage. Not needed for plain caged servers.
- **A registry login**, for publishing and pulling private bundles. `login` authenticates you to an OCI registry. Not needed to cage and run locally.

## Getting started

- [init](init.md): prepare the runtime; the one-time setup step.
- [import](import.md): cage an MCP server from npm, PyPI, or an OCI image, or compose several into a reasoning agent. The place almost everything begins.
- [config](config.md): set your model provider, resource caps, and daemon defaults.

## Cage a server

- [import](import.md): turn a server into a caged, content-addressed bundle.
- [egress](egress.md): approve or reject the hosts a running server reaches; a run is deny-default and holds a new host until you allow it.
- [build](build.md): rebuild a bundle from an edited source tree.
- [inspect](inspect.md): print a bundle's resolved manifest, tools, and directives.
- [tree](tree.md): show an agent's `USES` dependency graph and where `BAN` and `DENY` cut it.

## Run and serve

- [run](run.md): give an agent a task from your terminal and get one answer.
- [call](call.md): invoke a single tool of a caged server or agent directly.
- [serve](serve.md): put one or more servers, or an agent, behind one HTTP endpoint.
- [ps](ps.md): list running agents and their run ids.
- [stop](stop.md): end one or more runs and release what they hold.

## Distribute

- [push](push.md): sign a bundle and push it to an OCI registry.
- [pull](pull.md): pull and verify a published bundle.
- [search](search.md): find bundles in a registry, or in your local store.
- [register](register.md): list a bundle in an MCP registry for discovery.
- [login](login.md): authenticate to an OCI registry.
- [keys](keys.md): manage your signing key.
- [trust](trust.md): manage the publisher keys you have pinned on first pull.

## Watch and control

- [logs](logs.md): stream a run's logs, including the egress a server attempted.
- [events](events.md): follow the run lifecycle event stream.
- [stats](stats.md): watch live resource use per cage.
- [spend](spend.md): see what a run has cost against its budget.
- [budget](budget.md): read and reason about per-run spend caps.
- [trace](trace.md): inspect the tool-call and reasoning trace of a run.
- [replay](replay.md): re-run a recorded session.
- [daemon](daemon.md): manage the control-plane daemon.

## Store, secrets, and evaluation

- [store](store.md): inspect, load, and remove bundles in the local store.
- [secrets](secrets.md): store the secret keys servers need, kept out of logs and bundles.
- [eval](eval.md): run an agent's eval suite.

## Reference

- [VESSELFILE.md](VESSELFILE.md): every directive the manifest supports, what it means, and how the author's intent meets the operator's controls.
- [REASONER.md](REASONER.md): the reasoning harness `--reasoning` writes, its environment contract, and how to shape or replace it.
- [ARCHITECTURE.md](ARCHITECTURE.md): the cages, brokers, networks, and trust model a run is built from.

## Reporting and support

Bugs and features go to the [issue tracker](https://github.com/okedeji/mcpvessel/issues); security reports go privately, per [SECURITY.md](../SECURITY.md).
