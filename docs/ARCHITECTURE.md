# Architecture

## 1. Overview

mcpvessel executes Model Context Protocol (MCP) servers under the assumption that each one is hostile. An MCP server is ordinarily a subprocess with the user's full authority; mcpvessel instead runs each in a container, a *cage*, and grants it authority one piece at a time. The central guarantee is this: a caged server obtains no authority it was not explicitly granted, and every grant it does hold is exercised through a component the server can neither bypass nor forge.

Four mechanisms produce that guarantee, and the rest of this document is an account of them:

1. **Isolation.** Each server runs alone in its own container on its own private network, with no shared filesystem and no ambient route to anything.
2. **Complete mediation.** Every unit of authority a cage can exercise, a call to another agent, an outbound network byte, a model completion, a secret, passes through a small *broker* container built for exactly that grant.
3. **Capability addressing.** What a cage can reach, it reaches by an unforgeable random token, not by a name it could guess or construct.
4. **Verified identity.** A server's code is content-addressed and signed, and its declared capabilities are observed by running it, not trusted as claimed.

The reader interested in the security argument should read §2 (threat model), §3 (principles), §11 (invariants), and §12 (failure behavior). The reader interested in how a request actually flows should read §4 (system model), §6 (brokers), and §8 (lifecycles). Terms are defined at first use; a glossary of constants is in Appendix A.

## 2. Threat model

**Adversary.** The adversary is arbitrary code executing inside a cage. It may issue any system call the kernel permits, attempt any network connection, and emit any MCP or tool-call traffic its position allows. It may be a server the operator installed today and an updated, now-malicious version of that same server tomorrow. It may also be a *published bundle*, an agent an author distributed and the operator pulled, whose author is the adversary; the code and its declared metadata are therefore untrusted until verified.

**Assets under protection.**

- The host filesystem and the operator's credentials on it (SSH keys, cloud configuration, environment files).
- The LLM provider key, which pays for and authorizes model calls.
- The other cages in a run and the inputs handed to them; one caged server must not reach another's tools, network, or secrets.
- The per-run spend budget, a finite economic resource.
- The integrity of a bundle in distribution: an operator who pulls an agent must run the bytes its author signed.

**Trusted computing base.** The trusted computing base (TCB) is the set of components that must function correctly for the guarantees to hold; if any is compromised the model breaks, so it is kept small. Here it is the container runtime (containerd, BuildKit, nerdctl), the broker containers, and the daemon. On macOS a Lima virtual machine encloses all of them and forms a second boundary between the runtime and the host. Everything inside a cage is untrusted, including the server's own declared tools, egress needs, and dependency references, each of which is verified (§9) rather than believed.

**Non-goals.** The cage constrains *capability*, not *intent*, and the following are explicitly out of scope. A tool that returns a plausible lie in its result is not detected; mediation governs what a server can reach, not whether its output is honest. A server granted a host and a matching secret may, by construction, use them together; that is the grant working as issued, not a breach. Micro-architectural and timing side channels are not addressed. A host already compromised outside mcpvessel is outside the model. These reappear in §14 as consequences an operator must own.

## 3. Design principles

**Least authority, deny by default.** A cage begins able to reach nothing. Every capability is added explicitly, and the absence of a grant resolves to a refusal or a held request awaiting one, never to a silent allow. This principle produces the network, egress, and secret invariants below.

**Complete mediation.** Each distinct kind of authority passes through exactly one broker built for it: inter-agent calls through the MCP gateway, outbound bytes through the egress proxy, model calls through the LLM gateway. A broker is a *reference monitor* (§6): it sees every exercise of its grant, and there is no path around it.

**Capabilities over names.** Routable authority is a 128-bit random token, minted per run and dead at teardown, not a guessable identifier. A cage cannot address a peer, a model route, or a control surface it was not handed the token for, and a token from one run means nothing in another.

**Verified identity, not declared identity.** A bundle's identity is the hash of its contents; its author is an ed25519 signature; its capabilities are read by booting it and asking. Metadata is observed or cryptographically bound, never accepted because the server stated it.

**Fail closed.** Every timeout, missing input, ambiguous identity, or crashed component resolves toward less authority: a held connection past its deadline is refused, a run that cannot establish its isolation does not boot, a daemon that died leaves orphans its successor sweeps. §12 enumerates these paths.

## 4. System model

An **author** builds and signs a bundle; an **operator** runs it. A **bundle** is a content-addressed, signed artifact holding an agent's source and manifest. A **cage** is one OCI container running one MCP server. A **run** is the set of cages, networks, and brokers started together under one run id and torn down together; `run`, `call`, and `serve` each produce one. A **broker** is a container mcpvessel interposes between cages and a resource. An agent may declare dependencies on other published agents (`USES`), so a run is in general a **tree** of cages.

Figure 1 shows one run of a two-agent tree. Every later section refers back to it.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="figures/fig1-topology-dark.svg">
  <img alt="One run of a two-agent tree. The operator's CLI and the daemon run on the host, alongside the ~/.mcpvessel store; the cages (reasoner, sub-agents, tool collectors) and the brokers (MCP gateway, LLM gateway, egress proxy) run inside a Lima VM on macOS or directly on the host on Linux. Each cage is on its own private network. The reasoner reaches the MCP gateway for tool calls and the LLM gateway for model calls; the LLM gateway holds the provider key and reaches the provider directly, while the egress proxy filters all other outbound traffic deny-default. The daemon drives the VM via limactl shell and nerdctl exec." src="figures/fig1-topology-light.svg">
</picture>

*Figure 1. One run of a two-agent tree. The CLI, daemon, and `~/.mcpvessel` run on the host; the cages and brokers run in the Lima VM on macOS or on the host on Linux. Cages are untrusted; the brokers and daemon are the TCB, and each cage wall plus the VM wall (macOS) is a boundary. No two cages share a network. The reasoning cage reaches the MCP gateway (tool calls) and the LLM gateway (model calls) on separate URLs; the two brokers never talk. Cage egress is filtered by the proxy deny-default; the LLM gateway reaches the provider directly. Numbered arrows trace the tool-call lifecycle of §8.1.*

Three entry surfaces feed this topology and converge on it: `run` and `call` boot a run over a one-shot stdio session; `serve` opens a long-lived MCP-over-HTTP front door; and `serve` also answers plain REST for callers that speak no MCP. All three produce the same cage-and-broker graph behind the entry point, so the guarantees below hold regardless of how a run was started.

## 5. Isolation substrate

**One server, one container.** Each MCP server runs in its own OCI container with no shared filesystem, no shared process namespace, and no network it did not receive. A cage holds exactly the image it was built from, the inputs the operator supplied for this run, and one network interface.

**Per-cage networks (I1).** No two cages share a network. This is the substrate every other guarantee rests on: because a cage's only network peers are the brokers multi-homed onto its network, a caged server has no route to a sibling and therefore cannot reach one except through the MCP gateway, where mediation applies. The root cage and any always-warm cage hold a dedicated network. Cages that boot on demand draw a network from a small reusable pool, with one cage occupying a pool network at a time; the exclusivity holds across reuse, so a recycled network never carries two live cages at once. Pooling exists only to keep the number of networks bounded on a large tree, below the practical limits of the container network layer; it never relaxes exclusivity. A boot that cannot establish a cage's network fails rather than colocating it.

**The machine boundary.** On Linux the cages and brokers run on the host's own container runtime directly. On macOS, where there is no native Linux container support, mcpvessel provisions a lightweight Lima virtual machine on first `init` and runs the cages and brokers inside it, so the host never executes caged code and the VM is a second boundary around it. The daemon and the CLI remain host processes: the daemon drives the VM by invoking `limactl shell <vm> nerdctl` for every container operation, which is also why a cage, isolated inside the VM on an internal network, has no route back to the daemon (§6.2). The runtime is rootless. It uses containerd for image and container storage and BuildKit's `dockerfile.v0` frontend for builds, and it drives container lifecycle by invoking nerdctl as a subprocess rather than linking the containerd client, which keeps the runtime's supported surface equal to a documented command-line tool. None of this is a dependency the operator installs; the runtime carries it.

## 6. Mediation: brokers as reference monitors

A broker is a *reference monitor*: the classical requirements on such a component are complete mediation, tamper-proofness, and verifiability, and the design meets each concretely.

- **Complete mediation** follows from topology. I1 leaves a cage no route except to the brokers on its network, and each broker is the sole holder of the resource behind it, so every exercise of a grant is seen.
- **Tamper-proofness** follows from placement. Brokers run outside the network of every cage, so no caged server can reach a broker's administrative surface. Each broker's control interface binds to loopback inside the broker's own container and is reachable only by the daemon, which drives it with `nerdctl exec` from the host. A cage cannot address it at all.
- **Verifiability** follows from scope. Each broker does one job small enough to read and reason about: filter CONNECTs, route calls, hold a key. None terminates TLS it need not, and none holds state beyond the run.

The subsections below describe each broker in a fixed frame: what it *mediates*, what it *holds*, and why it *cannot be bypassed*.

### 6.1 MCP gateway

*Mediates* every tool call between agents in a run. *Holds* the routing table that maps each `USES` edge to its target cage, together with the `DENY` and `BAN` rules that edge carries. *Cannot be bypassed* because a calling agent is handed a `VESSEL_USES_<ALIAS>_URL` that addresses the gateway, never the sub-agent directly, and I1 guarantees no other route to that sub-agent exists. Every call in the tree therefore arrives at the gateway, which resolves the edge's capability token to a target, applies `DENY` (a per-edge tool refusal) and `BAN` (a subtree-wide refusal of an agent or its tools), and forwards only what survives. A cage cannot construct a peer's address, and could not reach it if it did.

### 6.2 Egress proxy

*Mediates* every outbound network connection a cage attempts. *Holds* the per-source allow-set and the set of connections currently held awaiting a decision; it filters on the CONNECT target without terminating TLS, so it holds no plaintext and no credential. *Cannot be bypassed* because the cage's network has no route to the internet except through the proxy, and the proxy is deny-default.

The proxy authorizes by source network address. This is sound only because a cage's address is unambiguous: per-cage networks (I1) make one address name one cage, the proxy refuses to start if two agents ever resolve to the same address ("refusing to mis-authorize egress", a fail-closed check), and egress-bearing cages are pinned warm so their address cannot drift mid-run.

A host absent from the allow-set is not refused outright. The proxy **holds** the connection and asks the operator, and only a genuine rejection or a lapsed deadline turns a hold into a refusal. A server whose Vesselfile declares `EGRESS deny-default` is the one exception: it runs with no proxy and no outbound path at all, hard isolation for a tool that should never touch the network.

The hold mechanism must cross the cage boundary to reach a human, and it does so through a deliberate asymmetry, shown in Figure 2. The proxy cannot dial the daemon: it sits on internal cage networks, and the daemon is on the host, outside them. So the two directions of the approval conversation travel by different means and never share a channel.

```
  Figure 2. The egress hold and approval channel.

   cage ──CONNECT api.example──▶ ┌───────────────┐
                                 │ egress proxy  │  host not in allow-set
                                 │  HOLD conn ───┼──▶ "egress pending: api.example"
                                 └───────┬───────┘        (stdout)
                                         │                    │
        approve-in                       │ signal-out         ▼
   ┌───────────────┐                     │            ┌──────────────┐
   │ mcpvessel     │                     │            │ log pump ────┼─▶ durable log
   │ egress allow  │                     │            │ (scans lines)│
   └──────┬────────┘                     │            └──────┬───────┘
          │ daemon                       │                   │ event bus
          ▼ nerdctl exec                 │                   ▼
   ┌──────────────────┐                  │            terminal prompt [y/N]
   │ loopback control │──── release ─────┘            /  mcpvessel egress ls
   │ listener (:9005) │                                \ events feed
   └──────────────────┘
     approved ─▶ tunnel opens        denied / deadline ─▶ 403
```

Signal-out rides the container's own stdout: the proxy writes an `egress pending:` line, the daemon's log pump scans it, records the held host, and publishes an event that surfaces as a terminal prompt, an entry in `mcpvessel egress ls`, and an item on the events feed. Approve-in rides the reverse path: `mcpvessel egress allow` reaches the daemon, which execs the proxy's loopback control listener and releases (or, on `deny`, drops) the held connection. The two half-channels are wired through different transports precisely because the isolation model forbids a shared one; the asymmetry is the model holding, not a workaround. An approval is also persisted to operator config keyed by the agent's version, so a later run of the same agent reaches the host without a second hold.

### 6.3 LLM gateway

*Mediates* every model completion a reasoning agent requests, and exists only when a run contains a reasoning agent. *Holds* the operator's LLM provider key, which never enters any cage. *Cannot be bypassed* because a reasoner reaches a model only through `VESSEL_LLM_URL`, which addresses the gateway. It joins the reasoning cages' networks (so those cages can reach it) and the egress network (its own path out), but never a non-reasoning cage's network, so no non-reasoning cage shares a network with the key holder. The MCP gateway is not in this path: tool calls and model calls are separate URLs, and the two brokers never talk to each other.

The cage sends a request bearing a placeholder model name and a throwaway credential. The gateway substitutes the real provider key, rewrites the model field to the operator's configured choice, meters the token spend against the run's budget in integer micro-USD, and forwards the request to the provider. It reaches the provider directly over the egress network rather than through the egress proxy: the gateway is trusted and the provider is the operator's own configured endpoint, so it is not subject to the deny-default hold that governs a cage's egress. It routes by the per-agent capability token embedded in the URL, not by the agent's name, so one agent cannot address another's model route. A loopback control surface, driven like the others by `nerdctl exec`, lets the daemon adjust the budget on a live run.

### 6.4 MCP bridge

*Mediates* the transport mismatch between a server and the gateway. Most MCP servers speak the protocol over stdio; the gateway speaks streamable HTTP. *Holds* nothing; it is a thin adapter that wraps a cage's stdio server and serves it as streamable HTTP at `:8000/mcp`. It is *injected* into the image at build time from the host's own binary, in the target architecture, rather than shipped inside a published bundle. Two consequences follow: a bundle built on one machine runs on another because the correct-architecture bridge is supplied wherever it is built, and a published bundle carries the author's source and manifest but not an executable the author's machine produced for the bridge role.

Taken together, a cage's complete set of reachable peers is: the MCP gateway (to be called and to call its declared dependencies), the egress proxy if it has any egress at all, and the LLM gateway if it reasons. Nothing else: not the host, not the daemon, not the internet beyond its allow-set, not a sibling cage.

## 7. Capability and grant model

Authority in a run is handed out as capabilities and gated at injection. Figure 4 traces a secret's path; the same discipline applies to every grant.

**Capability tokens.** A capability token is sixteen bytes from a cryptographic random source, hex-encoded, minted per run and invalid after teardown. Both the LLM gateway routes and the MCP gateway edges are addressed by such tokens. Token addressing is chosen over name-based access control because a token is unforgeable (a cage cannot compute one it was not given), unguessable (128 bits of entropy), and non-transferable across runs (a token names nothing in a run that did not mint it). A name, by contrast, is exactly the kind of value a hostile cage can construct.

**Secrets.** Two stores are kept distinct. Secret *values* live in an operator store, entered only over stdin, never as a command argument; the store type redacts itself in its `String`, `GoString`, and `MarshalJSON` forms, so a value cannot leak through an incidental log or serialization. Secret *bindings*, which agent receives which named secret, live in operator config and hold names only, never values. At run time a value is injected into a cage only if that cage's manifest declares the name in `SECRETS`; a broadcast grant therefore cannot leak into a server that never asked for it. Grants can be narrowed from the broadcast pool to a single agent, and the runtime warns on the one grant shape that resembles credential harvesting: a single secret broadcast to a run in which more than one agent declares it.

```
  Figure 4. Secret value and grant flow.

   stdin ──▶ [ secret store ]        (value; self-redacting; ~/.mcpvessel/secrets.json)
                   │
                   │ resolved at run time, by name
                   ▼
   config binding: "@org/name:ver -> [NAME]"   (name only; never the value)
                   │
                   ▼
   run-time pool ──▶ declaration gate: inject NAME only if manifest SECRETS lists it
                   │
                   ▼
              cage env

   Never happens:   value written into a bundle   ✗
                    value written into config      ✗
```

**Egress grants.** A cage's outbound allow-set is the union of four sources: hosts the author baked into the Vesselfile `EGRESS allow:`, hosts the operator passed with `--egress` for this run, hosts persisted in operator config, and hosts approved live during the run (§6.2). Config and live approvals are keyed to the agent's exact version, so a version change re-asks rather than silently carrying an old decision forward.

**Elicitation.** A reasoning agent may ask the operator a question mid-call. The channel is advertised only when a handler is bound to answer, so an agent that tries to ask when no one can answer fails closed rather than hanging, and the wait is bounded by a deadline. The same mechanism underlies the interactive egress prompt.

## 8. Request lifecycles

### 8.1 A tool call across a tree

Following the numbered arrows in Figure 1:

1. A client sends a call to the front door (a `run` session or a `serve` endpoint), which routes it to the root cage's reasoner.
2. The reasoner decides to use a dependency and calls the URL it was injected, `VESSEL_USES_<ALIAS>_URL`, which addresses the MCP gateway.
3. The gateway resolves the edge's capability token to the sub-agent cage, applies `DENY` and `BAN`, and on success forwards the call to that cage's bridge at `:8000/mcp`.
4. If the reasoner needs a model completion to decide, it calls `VESSEL_LLM_URL`; the LLM gateway substitutes the real key, meters spend, and forwards upstream.
5. The sub-agent executes the tool and returns its result back through the gateway to the reasoner.
6. Any outbound network the sub-agent attempts during the tool passes the egress proxy (§8.2).
7. The reasoner's final result returns through the front door. Throughout, the run's durable log records tool activity and egress decisions, and spend accrues against the budget.

### 8.2 An egress decision

1. A cage issues a CONNECT to the egress proxy for some host.
2. The proxy checks its allow-set. On a hit, the connection tunnels immediately and an `egress allowed:` line is logged once.
3. On a miss, the proxy registers a hold, writes an `egress pending:` line to stdout, and blocks the connection.
4. The daemon's log pump scans the line, records the held host, and publishes an `egress.pending` event; the operator sees it as an inline prompt at an attached terminal, as a row in `mcpvessel egress ls`, and on the events feed, each carrying the exact approve command.
5. The operator answers. `mcpvessel egress allow` (or a `y` at the prompt) reaches the daemon, which execs the proxy's loopback control listener.
6. The listener releases the held connection, which now tunnels, and adds the host to the runtime allow-set so a later attempt this run does not hold again. The approval is persisted to config; a `deny`, or a lapse of the hold deadline, instead returns a 403 to the cage, and the tool error is enriched to name the blocked host. A subsequent run reads the persisted host and does not hold.

## 9. Identity, build, and distribution

A bundle's identity and provenance are established by construction, not assertion. Figure 3 shows the derivation.

```
  Figure 3. Identity derivation.

  source tree ──sha256──▶ files hash ───────────────▶ bundle identity
                              │                         (re-hashed on every extract:
                              │                          a tampered bundle is rejected)
                              ▼
  files hash + codegen fingerprint + bridge fingerprint ──sha256──▶ image tag
                              │
                              ▼
  OCI manifest digest + repository ──ed25519──▶ signature ──▶ TOFU pin (first pull)
                                                                 │
                                              later pull key mismatch ─▶ fail closed
```

**Content addressing.** A bundle's identity is the hash of its files, so identical source yields an identical bundle and a pull can confirm it received exactly those bytes. Extraction re-hashes the tree against the manifest, so a bundle altered in transit or at rest is rejected on load.

**Image identity.** The built image's tag folds three inputs into one digest: the files hash, a fingerprint of the code-generation logic, and a fingerprint of the injected bridge. A change to how mcpvessel generates or bridges an agent therefore changes the tag and forces a rebuild, rather than silently reusing an image built by older logic.

**Verification by behavior.** A bundle's tool catalog is not taken from the author's word. The server is booted in a cage at build time and introspected, and the catalog is what it actually advertised; a declared `MAIN` tool is checked to exist there. A `USES` dependency must name a concrete version and may not use a floating `latest`, so a dependency cannot drift out from under the parent's content hash.

**Signing and trust.** Every bundle is signed on push and verified on pull. The signature is ed25519 over the bundle's OCI manifest digest together with the repository it was published to, so a valid signature cannot be replayed onto a different repository. Trust is established on first use: the first pull of a publisher pins the key it sees and prints its fingerprint, and every later pull must match that pin, with a changed key failing closed and loudly rather than swapping silently, the model SSH uses for host keys. A signature proves origin, not intent; it attests who built a bundle, and the sandbox, not the signature, is what contains a bundle that proves malicious. The full policy is in [SECURITY.md](../SECURITY.md).

## 10. Resource and economic governance

Isolation protects confidentiality and integrity; a hostile cage can still attempt to exhaust the machine or the budget, and those are bounded too.

**Resource caps.** Each cage runs under cpu, memory, and pid caps enforced by the runtime. The enforced value resolves in order: a per-agent operator cap, then the operator's default cap, then a built-in default; a cage is always capped by at least one of these. An author's `RESOURCES` directive is advisory only and never the enforced value, so a bundle cannot raise its own ceiling.

**Admission and capacity.** Before a cage boots, its memory is admitted against the machine's real available memory, adjusted by the operator's machine setting; a run that would overcommit is reduced rather than allowed to thrash. A per-run ceiling bounds how many cages a single run may hold live at once, and a host-wide ceiling bounds cages across all runs. Idle cages are reaped after a configurable interval, and the operator can trade footprint for latency by prewarming a root's direct children or keeping named agents warm.

**Budget.** LLM spend is metered at the gateway in integer micro-USD, avoiding floating-point drift, and enforced as a hard ceiling: a run that exhausts its budget has the offending call fail, and its terminal record is escalated to an over-budget status. An author's `BUDGET` is advisory; the operator's `--budget` is the enforced cap.

**Serve multi-tenancy.** Under `serve`, each connected client receives its own whole agent instance, its own cage tree and conversation state, so clients are isolated from one another as well as from the host. The number of concurrent client instances per served agent and the idle interval before an abandoned one is reclaimed are both bounded, which caps load as well as separating tenants.

## 11. Security invariants

The guarantees above are stated once more as invariants, each with the mechanism that enforces it and the behavior when it cannot hold. An invariant that cannot be established prevents the run rather than degrading it.

| # | Invariant | Enforced by | On violation |
| --- | --- | --- | --- |
| I1 | No two cages share a network, so a cage's only peers are its brokers. | Dedicated or pool-exclusive internal network per cage (§5). | A boot that cannot place a cage on its own network fails rather than colocating it. |
| I2 | Every inter-agent tool call transits the MCP gateway, where `DENY` and `BAN` apply. | Callers hold a gateway capability URL, never a peer address; I1 removes any alternate route (§6.1). | No addressable route to a peer exists, so an unmediated call cannot be made. |
| I3 | No outbound byte leaves a cage except to a host in its allow-set or one the operator approves. | The egress proxy is the sole route out and is deny-default with hold (§6.2). | An unapproved host is held, then refused at the deadline; a `deny-default` cage has no proxy at all. |
| I4 | Each cage maps to exactly one egress source identity. | Per-cage subnets make an address name one cage; the proxy refuses to start on a duplicate address; egress cages are pinned warm (§6.2). | Ambiguous identity aborts proxy startup, failing the run closed. |
| I5 | An allowed hostname cannot pivot to a private or internal address. | The proxy resolves, filters to public addresses, and dials the checked address without re-resolving (§6.2). | A name resolving only to non-public addresses is refused. |
| I6 | The LLM provider key never enters a cage, and no non-reasoning cage shares a network with the key holder. | The LLM gateway holds the key, substitutes it for a placeholder, and joins only reasoning networks (§6.3). | A cage has no route to the key holder and no key to send. |
| I7 | A cage can address only the peers, routes, and control surfaces it was handed a token for. | 128-bit `crypto/rand` capability tokens, minted per run, embedded in injected URLs (§7). | A token a cage was not given names nothing; a token from another run is invalid. |
| I8 | A secret value reaches only a cage whose manifest declares its name, and never enters a bundle or config. | Declaration-gated injection; stdin-only entry; a self-redacting store; name-only config bindings (§7). | An undeclared name is not injected; a value has no path into a shared artifact. |
| I9 | An operator runs exactly the bytes the author signed, and a changed publisher key is caught. | Content hash re-verified on extract; ed25519 over the manifest digest plus repository; trust-on-first-use pinning (§9). | A tampered bundle or a key mismatch fails the pull closed. |
| I10 | A cage cannot exceed its cpu, memory, or pid caps, nor a run its spend budget. | Operator-enforced caps (author values advisory), pre-boot memory admission, a hard micro-USD budget ceiling (§10). | An overcommit is reduced before boot; a budget-exhausting call fails and the run is marked over-budget. |

## 12. Failure behavior

Every failure path resolves toward less authority. The table records the intended behavior and why it is safe.

| Event | Behavior | Why it is safe |
| --- | --- | --- |
| Egress to an unapproved host | Connection held, operator asked | No byte leaves until a human allows it |
| Hold deadline lapses | Connection refused (403) | An unanswered request fails closed, and the cage is freed |
| Agent elicits with no answer channel | Elicitation errors, call fails | No silent hang; authority is not assumed |
| Required secret or env missing | Boot fails | A server never runs half-configured with a partial grant |
| Provider key missing | Reasoning run refused | The key is required to exist, not fabricated |
| Two cages resolve to one proxy address | Proxy refuses to start | Ambiguous identity would mis-authorize egress |
| Budget exhausted | Call fails, run marked over-budget | Spend cannot exceed the ceiling |
| Daemon crash leaves orphans | Successor sweeps by label, containers before networks | Orphaned cages and networks do not accumulate or leak |
| Stale runtime build detected | Operator warned to restart | A daemon and CLI of different builds do not silently disagree |
| Run teardown | Front door closes, terminal record written while the gateway lives, then cages released | Final spend is captured before the component that reports it is gone |

## 13. Auditability

A reference monitor that cannot be observed is only half a monitor, so every decision leaves a record. The durable per-run log captures every egress decision, allowed, denied, and pending, and is readable live or after the run with `logs`. A lifecycle and approval event feed carries run start and end, egress pending and approved, and elicitation asked and answered. Spend is recorded per run. For maximal fidelity, a run can capture full LLM request and response payloads for later `replay`. The event feed is intentionally lossy under backpressure: a subscriber that falls too far behind loses events rather than stalling the run that produces them, because a stuck observer must never block a live run.

## 14. Limitations and non-goals

The boundaries of the guarantee, stated plainly so an operator can reason about them.

- An allowed host receives whatever the server sends it. Egress control decides *which* hosts, not *what* is sent to one that is permitted.
- A server granted a secret and a matching egress host can use them together. That is the grant working as issued; scope the grant narrowly if the combination is not intended.
- A signature proves who built a bundle, not that the bundle behaves. Origin is attested; intent is contained by the sandbox, not vouched for by the key.
- Trust on first use trusts the first key it sees. A first pull over a compromised channel pins the wrong key; verify the printed fingerprint against the publisher's stated one.
- An interactive egress approval consumes a time-boxed hold, not an indefinite one. An operator who never answers gets a refusal, not a hang, which is safe but is not the same as a decision.
- In-protocol deception, side channels, and a host compromised outside mcpvessel remain out of scope (§2).

## Appendix A. Constants

| Constant | Value |
| --- | --- |
| Capability token width | 128 bits (16 random bytes) |
| Bridge / sub-agent MCP port | 8000, path `/mcp` |
| MCP gateway port | 9000 |
| LLM gateway port | 9001 |
| Egress proxy port | 9002 |
| LLM gateway control port (loopback) | 9003 |
| MCP gateway control port (loopback) | 9004 |
| Egress proxy control port (loopback) | 9005 |
| Egress hold deadline | 3 minutes |
| Elicitation deadline | 3 minutes |
| Default max client instances per served agent | 8 |
| Default client idle TTL | 900 seconds |
| Default cages prewarmed per run | 2 |
| Default max live cages per run | 12 |
| Default host-wide max live cages | 128 |
| Default cage idle TTL | 300 seconds |

## Appendix B. Lineage

The design assembles established ideas rather than inventing them. The brokers are reference monitors in the sense of the Anderson report: complete mediation, tamper-proofness, verifiability. Addressing by unforgeable token is object-capability discipline, authority as an unguessable reference rather than an entry in an access-control list keyed by name. First-use key pinning is the trust model SSH uses for host keys. The interactive, deny-default egress prompt is the model of a personal firewall such as Little Snitch, applied to a container's outbound connections rather than a desktop's. The contribution is the composition: these placed together so that an untrusted MCP server can be run, composed, and distributed without extending it the authority of the process that hosts it.

## See also

- [How it works, briefly](../README.md#how-it-works-briefly): the one-paragraph version this document expands.
- [VESSELFILE.md](VESSELFILE.md): the `EGRESS`, `USES`, `BAN`, `SECRETS`, `RESOURCES`, and `BUDGET` directives that configure the mechanisms above.
- [REASONER.md](REASONER.md): how a reasoning agent reaches the MCP and LLM gateways, and why it holds no key.
- [egress](egress.md): the operator's view of the deny-default hold and approval flow of §6.2.
- [daemon](daemon.md), [init](init.md): the control plane of §12 and the runtime setup of §5.
- [SECURITY.md](../SECURITY.md): the full signing, trust, and disclosure policy.
