# config

Read and write `~/.mcpvessel/config.json`: the LLM endpoints the gateway routes to, the resource caps the runtime enforces on every cage, how a run's cage tree is kept warm, how a served agent is scaled, how big a machine mcpvessel may claim, and where the daemon serves metrics. Every value is set through a subcommand and validated before it lands on disk. Secrets never live here: a provider key is stored by reference, and the file names the secret rather than holding it.

```
mcpvessel config <subcommand> ...
```

The file is optional. A missing one reads as an empty config, so before you set anything every knob is at its built-in default. A malformed file is an error, not a silent reset, so a typo never quietly drops your providers. Each `set` loads the file, applies your change, revalidates the whole config, and rewrites it at mode `0600` under a `0700` directory. The path follows `VESSEL_HOME` when that is set, so all of mcpvessel's state moves together; otherwise it is `~/.mcpvessel/config.json`.

Numeric policy knobs share one convention: zero means "use the built-in default", and a negative value is rejected at save time rather than read as "unlimited". So clearing a knob back to its default means setting it to `0`, and there is no way to ask for no cap at all.

## provider

Configure the OpenAI-compatible LLM endpoints the gateway can route to. Each endpoint is a name, a base URL, an optional key reference into the secret store, an optional model to send, optional per-token pricing, and an optional default flag.

**`provider set NAME`** adds an endpoint or replaces the one with the same name. `--base-url` is required; without it the command errors before touching the file. `--key-ref` names a secret you stored with `mcpvessel secrets set`; the config records the name, never the value. `--default` marks this endpoint as the one used when an agent's own provider is not configured, and setting it clears the default flag on every other endpoint so exactly one default ever exists.

`--price-in` and `--price-out` are USD per million tokens, written like `2.50` or `0.003`. They are parsed into integer micro-USD so run cost stays exact integer math against a micro-USD budget. More than six decimal places is finer than mcpvessel tracks and is rejected; a non-numeric or negative amount is rejected too.

**`provider ls`** prints one line per endpoint: name, base URL, `key-ref=<name>` when set, `$IN/$OUT per Mtok` when either price is set, and `[default]` on the default. It prints the key reference, never a key value, because the value is not in the config to begin with.

**`provider rm NAME`** drops the named endpoint and errors if no endpoint by that name exists.

| Flag (on `set`) | Meaning |
| --- | --- |
| `--base-url URL` | OpenAI-compatible base URL. Required. |
| `--key-ref NAME` | Name of a secret (`mcpvessel secrets`) holding the API key. The key stays in the secret store. |
| `--model NAME` | Model name to send to this endpoint, used on fallback when an agent's provider is not this one. |
| `--price-in USD` | USD per million input tokens, e.g. `2.50`. Stored as micro-USD; max six decimal places. |
| `--price-out USD` | USD per million output tokens, same format. |
| `--default` | Route to this endpoint when an agent's provider is not configured. Clears any previous default. |

## resources

Set the cpu, memory, and pid caps the runtime hands to nerdctl for each cage. There is one default cap plus per-agent overrides keyed by `@org/name:version` registry ref. A per-agent cap only matches a pulled USES dependency that carries that ref. An agent you run straight from a `.agent` file has no registry ref to match, so it takes the default cap, or the runtime default when you have not set one. Every cage is capped one way or another.

**`resources set REF`** sets the cap for one agent, keyed by its `@org/name:version` ref. **`resources default`** sets the cap applied to every cage without a per-agent match. Both take the same flags and both require at least one of them. If you pass `--pids` it must be positive. `--cpus` must be a positive number and `--memory` a positive size, checked at save time.

**`resources ls`** prints the default cap (as `default`) when set, then each per-agent cap, showing only the fields you gave (`cpus=`, `mem=`, `pids=`).

**`resources rm REF`** removes one per-agent cap and errors if none exists for that ref. It does not touch the default cap; the default is only overwritten by another `resources default`, not cleared here.

| Flag (on `set` and `default`) | Meaning |
| --- | --- |
| `--cpus N` | nerdctl `--cpus` cap, e.g. `2` or `0.5`. Must be a positive number. |
| `--memory SIZE` | nerdctl `--memory` cap, e.g. `2g` or `512m` (k/m/g suffixes, base 1024). Must be a positive size. |
| `--pids N` | nerdctl `--pids-limit` cap. Must be positive when set. |

## egress

Set operator egress allow-lists, so a host you always allow is not passed with `--egress` or approved by hand every run. There is one general default plus per-agent lists keyed by `@org/name:version` (or `@org/name` to match any version). These hosts are added on top of what a bundle's own `EGRESS` declares; they only widen your own runs and never change a published bundle. This is the same store an interactive `mcpvessel egress allow` writes to, so an approved host lands here automatically.

**`egress set REF HOST...`** sets the allow-list for one agent. **`egress default HOST...`** sets the general list applied to every agent. **`egress ls`** prints the default then each per-agent list. **`egress rm REF`** drops a per-agent list (`rm default` clears the general one). Hosts may be space or comma separated.

## secrets

Bind the secret names an agent should receive, so you do not re-pass `--secret` every run. Keyed the same way as `egress`: a per-agent binding on `@org/name:version` (or `@org/name`), plus a general default. Only the name is stored here; the value resolves from your secret store (`mcpvessel secrets set NAME`) at run time, and a server only ever receives a name it declares in `SECRETS`, so a general binding cannot leak into a server that did not ask for it.

**`secrets set REF NAME...`**, **`secrets default NAME...`**, **`secrets ls`**, and **`secrets rm REF`** mirror the `egress` subcommands. Scoped bindings are the safe norm; the general default is the convenience.

## models

Override which model an agent uses, keyed by its `@org/name` registry ref, for example to pin an expensive agent to a cheaper model. Like a resource cap, an override only matches a pulled USES dependency with that ref. An agent you run directly from a `.agent` file has no ref to match, so its model comes from its own MODEL directive and the default provider.

**`models set REF PROVIDER/MODEL`** pins the ref to a `provider/model` string, overriding the agent's advisory MODEL. **`models ls`** lists every override as `ref  provider/model`. **`models rm REF`** clears the override for a ref by deleting its entry. `rm` does not check that an override was present first, so it prints its confirmation whether or not one existed.

## cages

Set how a single run's USES tree is kept warm: how many cages may be live at once, how many of the root's direct children to boot up front, and how long an idle cage lives before it is reaped. All of it is one `cages set`, which requires at least one flag. A change takes effect on the next run.

- `--max-live` caps the elastic cages within one run. Kept-warm cages do not count against it. Default `12`.
- `--host-max-live` caps cages across every run on the host, where every cage counts. It is the harder ceiling underneath the per-run cap. Default `128`.
- `--prewarm` is how many of the root's direct children boot up front rather than lazily. Default `2`.
- `--idle-ttl` reaps a cage that has sat idle longer than this many seconds. Default `300` (five minutes).
- `--keep-warm REF` names an agent ref to keep booted even when idle, distinct from the automatic pinning of a cage mid-call. It is repeatable, and each `cages set` replaces the whole list rather than appending, so pass every ref you want in one invocation.

| Flag | Meaning | Default |
| --- | --- | --- |
| `--max-live N` | Max elastic cages per run; kept-warm cages excluded. | 12 |
| `--host-max-live N` | Max cages across every run on the host. | 128 |
| `--prewarm N` | Root's direct children booted up front. | 2 |
| `--idle-ttl SECONDS` | Reap a cage idle past this. | 300 |
| `--keep-warm REF` | Ref kept booted even when idle. Repeatable, replaces the list. | (none) |

## serve

Set the policy for `mcpvessel serve`, which is a level above `cages`. Each connected client gets its own whole agent instance, meaning its own cage tree plus conversation state, and this policy governs those whole instances. One `serve set`, at least one flag.

- `--max-clients` caps concurrent client instances per served agent. Default `8`.
- `--client-idle-ttl` reaps an instance whose client has gone quiet past this many seconds, freeing an abandoned session without cutting off a human mid-chat. Default `900` (fifteen minutes).

| Flag | Meaning | Default |
| --- | --- | --- |
| `--max-clients N` | Concurrent client instances per served agent. | 8 |
| `--client-idle-ttl SECONDS` | Reap an instance idle past this. | 900 |

## machine

Set how much of the host mcpvessel may claim for cages. One `machine set`, at least one flag. A change applies when the machine is next provisioned, so recreate it with `mcpvessel init --recreate` (the command reminds you). Zero on any field means the built-in default.

The fields behave differently by platform. On macOS all three size the Lima VM the cages run in: `--memory-gib`, `--cpus`, and `--disk-gib`. On Linux there is no VM: `--memory-gib` caps the host RAM that cages are admitted against, and `--cpus` and `--disk-gib` are ignored. The zero default is a 4 GiB VM on macOS and the whole host on Linux.

| Flag | Meaning |
| --- | --- |
| `--memory-gib N` | Memory in GiB. Sizes the VM on macOS, caps admitted host RAM on Linux. |
| `--cpus N` | vCPUs for the macOS VM. Ignored on Linux. |
| `--disk-gib N` | Disk in GiB for the macOS VM. Ignored on Linux. |

## metrics

Configure where the daemon serves Prometheus metrics. The endpoint is on by default at `127.0.0.1:9323`, loopback only, reachable just from the same host. A change takes effect when the daemon next starts. This subcommand has its own three verbs.

**`metrics set ADDR`** binds the endpoint to `host:port`. The address is validated as `host:port` and rejected otherwise. If the host is not loopback, the command warns that the endpoint has no auth of its own, so you must restrict the port at the network layer (a security group, a private subnet, a VPN) since that network boundary is the only access control. Then it reminds you to restart the daemon.

**`metrics off`** disables the endpoint by storing the literal `off`.

**`metrics show`** prints where metrics are served, resolving the effective address: an unset value shows the default `http://127.0.0.1:9323/metrics`, and `off` (also `none` or `disabled`, if the file was hand-edited to those) shows `disabled`.

## env

Persist a `VESSEL_*` setting in the config so it survives across shells without an export, for example the MCP Registry login's GitHub client id. These are non-secret settings, not credentials; a credential belongs in `mcpvessel secrets`. A real environment variable of the same name always overrides the stored value, and a blank environment variable counts as unset so an exported empty value does not blank out a good stored one.

**`env set NAME VALUE`** stores a knob. **`env ls`** lists stored knobs sorted by name. **`env rm NAME`** removes one and errors if it was not set.

## show

Print the whole config as indented JSON. Since the file only ever holds key references and never secret values, `show` is safe to read: it exposes base URLs, key reference names, prices, caps, and policy, but no keys. The JSON keys mirror the on-disk schema (`providers`, `resources`, `models`, `cages`, `machine`, `serve`, `telemetry.metrics_addr`, `env`), and empty sections are omitted.

## path

Print the resolved config file path, honoring `VESSEL_HOME`. Useful for scripting or for editing the file directly.

## Examples

```sh
# Point the gateway at OpenAI, keyed by a stored secret, as the default provider.
mcpvessel secrets set openai_key
mcpvessel config provider set openai \
  --base-url https://api.openai.com/v1 --key-ref openai_key \
  --price-in 2.50 --price-out 10.00 --default

# Cap one pulled agent tighter than the default, and set a floor for everything else.
mcpvessel config resources default --cpus 1 --memory 1g
mcpvessel config resources set @okedeji/researcher:0.1 --memory 2g --cpus 2 --pids 512

# Pin an expensive agent to a cheaper model.
mcpvessel config models set @okedeji/researcher:0.1 openai/gpt-4o-mini

# Expose metrics to an off-host Prometheus, then restart the daemon.
mcpvessel config metrics set 0.0.0.0:9323
mcpvessel daemon restart
```

## Notes

- Every `set` revalidates the entire config before writing, so an edit that would leave two default providers, a duplicate provider name, negative pricing, or a bad cap fails the save and leaves the file untouched. A config already broken by hand editing surfaces the same error on the next save.
- The `set` verbs under `cages`, `serve`, and `machine` refuse to run with no flags, telling you which flags they accept. `resources set` and `resources default` refuse likewise.
- Removal verbs are not uniform. `provider rm`, `resources rm`, and `env rm` error when the target is absent; `models rm` does not, and reports success regardless.
- `resources rm` only removes a per-agent cap. The default cap is not removable through the CLI; overwrite it with another `resources default`, or hand-edit the file.
- `--key-ref` records a name only. If the named secret does not exist the config still saves; the gateway resolves it at run time, so a missing secret surfaces when an agent actually calls the provider, not here.
- Policy changes are not retroactive. `cages` and `serve` apply on the next run or serve, `metrics` when the daemon next starts, and `machine` only after `mcpvessel init --recreate` reprovisions the machine.
- A negative value passed to a numeric knob is caught at save time, not at flag parse. The flag accepts it, then `Save` rejects it with a message naming the field.

## See also

- [serve](serve.md): the served-agent scaling this page's `serve` policy governs.
- [egress](egress.md): approve held hosts at run time; `config egress` and `egress allow` share one store.
- [run](run.md): where the `cages`, `resources`, and `models` policies take effect.
- [init](init.md): provisioning the machine, and `--recreate` to apply a `machine` change.
- [daemon](daemon.md): the daemon that serves the metrics endpoint and must restart to pick up a `metrics` change.
- [login](login.md): the MCP Registry login that reads `VESSEL_GITHUB_CLIENT_ID`, an `env` knob.
- [give it a brain](../README.md#give-it-a-brain) in the README: configuring a provider before running a reasoning agent.
