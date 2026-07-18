# Architecture

The README explains what the cage does in a paragraph. This is the longer version: the containers a run is made of, the networks that isolate them, the brokers that are the only way out, and where the whole thing sits on your machine. If you want to know why a caged server cannot reach your files, why its token cannot leave, and why an agent never sees your model key, the answer is in the topology below.

## The unit of isolation is a container

Every MCP server mcpvessel cages is one OCI container, and every agent is a tree of them. There is no shared process, no shared filesystem, and no ambient network. A container gets exactly the image it was built from, the inputs the operator supplied for this run, and a single network with no route off it. Everything a server is allowed to do, it does through a door that mcpvessel runs and controls.

A "run" is that set of containers plus their networks, started together under one run id and torn down together. `run`, `call`, and `serve` all produce one. The daemon holds the registry of live runs; `ps` lists them, `stop` ends them.

## The cages and the networks

Each cage sits alone on its own internal-only network. "Alone" is the load-bearing word: because no two cages share a network, one caged server has no route to another, so it cannot skip the broker and call a sibling directly. "Internal-only" means the network has no path to the internet or your host by default. A plain caged server, given no egress, can talk to nothing but the broker that fronts it.

For a tree of agents the picture is the same repeated: the root agent and any always-warm cage each hold a dedicated network, and cages that boot on demand draw a network from a small reusable pool, one cage to a pool network at a time so the isolation holds even as networks are reused. The point of the pooling is only to keep the number of networks bounded on a large tree; it never lets two live cages share one.

## The brokers are the only doors

Between the cages and everything outside them sit a few small broker containers that mcpvessel runs for you. They are the entire attack surface a caged server has, and each one enforces one thing.

**The MCP gateway** is the referee for every tool call inside a run. It is the one container that joins every cage's network, and it is how a caller reaches a sub-agent's tools at all: each `USES` edge becomes one route on the gateway, and the caller is handed a `VESSEL_USES_<ALIAS>_URL` that points at the gateway, not at the sub-agent. So every call in the tree passes through the referee, which is where `DENY` and `BAN` are enforced. A cage cannot route around it, because a cage cannot see any network but its own.

**The egress proxy** is the outbound filter, and it is deny-default. An egress proxy joins each server's network and lets through only the hosts that server is allowed (its baked `EGRESS allow:`, the operator's `--egress` or `config egress`, and any live approval). A host that is not allowed is not refused outright: the proxy **holds** the connection and signals the daemon, which asks the operator to approve or reject it (`mcpvessel egress allow`), then releases or drops the held connection through a loopback control surface the daemon drives with `nerdctl exec`. Only a server whose Vesselfile declares `EGRESS deny-default` runs with no proxy and no outbound path at all.

**The LLM gateway** is the key holder, and it exists only when a run has a reasoning agent. It holds your model provider's key, which never enters any cage. A reasoner reaches it through `VESSEL_LLM_URL`, sends a placeholder model and a throwaway key, and the gateway rewrites the model to your configured default, meters the spend against the run's budget, and forwards the request to the real provider. Two properties keep this safe. The gateway routes by an unguessable token baked into each agent's URL, not by the agent's guessable name, so one agent cannot address another's LLM edge. And it joins only the networks of reasoning cages and the reasoning pool, so a non-reasoning cage never shares a network with the container that holds the key.

**The MCP bridge** is how a stdio MCP server becomes an HTTP one the gateway can reach. Most MCP servers speak MCP over stdin and stdout; the gateway speaks streamable HTTP. The bridge is a small companion binary that wraps the server's stdio and serves it as streamable HTTP on port 8000 at `/mcp`. It is injected into the image at build time from your host, in the target architecture, rather than baked into a published bundle, so a bundle built on one machine runs on another. That is why a bundle carries its source and manifest but adopts the correct bridge wherever it is built.

Put together: a caged server can reach the MCP gateway (to be called), maybe an egress proxy (if granted hosts), and, if it reasons, the LLM gateway (to think). Nothing else. Not your disk, not your network, not another cage.

## Where it all runs

On Linux, the containers run on the host's own container runtime directly. On macOS there is no native container support, so mcpvessel sets up a lightweight Linux VM with [Lima](https://lima-vm.org) on first `init`, and everything above runs inside it. Either way your host never runs the caged code directly; on macOS the VM is a second boundary around the whole system.

Under the hood the runtime uses containerd for image and container storage, BuildKit's `dockerfile.v0` frontend for builds, and drives container lifecycle by shelling out to nerdctl. None of this is something you install separately: the runtime brings its own, which is why there is no Docker or container engine to set up.

## Builds and identity

`build` turns a source tree and its Vesselfile into a bundle through BuildKit into the containerd image store. The bundle is content-addressed: its identity is the hash of its files, so the same source always produces the same bundle, and a pull can verify it got exactly those bytes. The built image's tag folds in more than the files, the codegen fingerprint and the injected-bridge fingerprint too, so a change to how mcpvessel generates or bridges an agent forces a rebuild rather than silently reusing a stale image. That fix is why a caged server runs correctly across machines and across mcpvessel versions.

## The control plane

The daemon is the long-lived control plane. It holds the registry of live runs, owns the control socket that `stop`, `ps`, `logs`, and the rest talk to, and runs a reconciliation sweep that reaps detached cages and networks a crashed or unreleased run left behind. This is why `stop` releases a run rather than just forgetting it, and why a clean shutdown and a per-run stop leave the same tidy state. `init` starts it (and on macOS sets up the VM first); `daemon` is the command that manages it.

## Signing and trust

Distribution rests on signatures, not on trusting a registry. Every bundle is signed on `push` and verified on `pull`. The first pull of a publisher pins the key it sees and prints its fingerprint; every later pull must match that pin, and a changed key fails closed with a loud error rather than swapping silently. This is trust on first use, the same model as SSH known hosts. Servers published from this project carry fingerprint `bf4894a180f2` (scope `ghcr.io/okedeji`); confirm it on your first pull. Signing proves origin, not intent: it tells you who built a bundle, and the sandbox, not the signature, is what contains a bundle that turns out to be malicious. The full policy is in [SECURITY.md](../SECURITY.md).

## What the architecture does and does not buy you

What it buys: a caged server runs with no access to your host or files, no outbound network beyond what you allow, and no provider key inside the sandbox; siblings cannot reach each other; a shared bundle carries no one's secrets; and you can verify you run what its author built. What it does not: a signature does not vouch for a server's behavior, an allowed host can still receive whatever a server sends it, and a server you grant a secret and a matching egress can, by design, use them together. The cage constrains capability, not intent. The README's [what it does not protect against](../README.md#what-it-does-not-protect-against) is the honest list.

## See also

- [How it works, briefly](../README.md#how-it-works-briefly): the one-paragraph version this page expands.
- [VESSELFILE.md](VESSELFILE.md): the `EGRESS`, `USES`, `BAN`, and `SECRETS` directives that configure the wiring above.
- [REASONER.md](REASONER.md): how an agent reaches the MCP and LLM gateways, and why it holds no key.
- [daemon](daemon.md), [init](init.md): the control plane and the runtime setup this page describes.
- [egress](egress.md): approve or reject the hosts a server is held on at the proxy layer.
- [SECURITY.md](../SECURITY.md): the full signing, trust, and scope policy.
