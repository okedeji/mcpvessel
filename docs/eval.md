# eval

Run the eval suite an agent declares with `EVAL` and report the results. Each case runs the agent in a real cage, the same one-shot path `mcpvessel run` takes, then checks the output against the expectations the case declares: substrings it must or must not contain, an exact match, a regexp, a cost ceiling, a duration ceiling, and an optional LLM judge that scores the output against a rubric. A full run records its pass/fail counts and judge score into the bundle's manifest, so `inspect` and anyone who pulls the agent can see them.

```
mcpvessel eval TARGET [flags]
```

Because every case boots the agent and runs its tool for real, an agent that spends LLM tokens spends real money on each case. `eval` is not a dry run.

## What TARGET can be

TARGET names the agent to evaluate. `eval` resolves it four ways.

- **A reference** you tagged at build (`@okedeji/researcher:0.1`), resolved through your store.
- **The content hash** an untagged build printed.
- **A path to a `.agent` file.**
- **A source directory** (`.`). `eval` builds it into your store first and evaluates the bundle it just built. The build skips introspection (it does not boot the server to re-list its tools), and it does not receive your `--env` or `--secret` inputs. Those feed the cases when they run, not this build. The display name for a directory build is the content hash, since it has no tag.

The suite always comes from a packed bundle, never straight from the source tree, which is why a directory is built first. Whichever form you pass, `eval` reads the bundle's manifest, requires an `EVAL` directive, and loads the suite the directive points at. A bundle with no `EVAL` directive fails with a message telling you to add one and rebuild.

## The suite

The `EVAL` directive points at a YAML file bundled with the agent. `eval` parses it with unknown fields rejected, so a typo like `output_containz` fails the load instead of silently skipping an expectation and reporting a false pass. The schema is locked to version `0.1`; any other `version` is refused. A suite with no cases, a case with no name, a duplicate case name, or a case with no `input.tool` all fail at load.

Each case names a tool to invoke and the arguments to pass it:

```yaml
version: "0.1"
cases:
  - name: summarize_short
    input:
      tool: respond
      args:
        goal: "Summarize the CHANGELOG in one sentence."
    expect:
      output_contains: ["release"]
      max_cost_usd: 0.05
      max_duration_seconds: 60
```

The tool must be public on the bundle: its `MAIN`, or one of its `EXPOSE`d tools. A case naming a private or nonexistent tool fails before any boot, so it fails cheaply rather than after a full agent start.

A case passes only when every expectation it declares passes. An expectation it leaves unset is not checked. A case that errors, crashes, overspends, or times out is a failed case, not an abort: the suite always runs to the end, and `eval` exits non-zero if any case failed.

## Expectations

All of these live under a case's `expect:`. Each is optional and checked only when present.

- **`output_contains`** (list): every substring must appear in the output.
- **`output_not_contains`** (list): none of these substrings may appear.
- **`output_equals`** (string): the output must equal this exactly, with surrounding whitespace trimmed on both sides (a model told to "reply with only X" routinely adds a trailing newline). Setting it to `""` requires empty output, distinct from leaving it unset.
- **`output_matches`** (list of RE2 patterns): the output must match each one. Patterns are compiled when the suite loads, so a broken pattern is a load error, not a case that silently never matches.
- **`max_cost_usd`** (number): a ceiling on the case's LLM spend. This is a soft cap. The gateway budget can let an in-flight call push spend a little past the ceiling and still complete, so a run that finished is post-checked against its recorded cost and fails if it went over.
- **`max_duration_seconds`** (integer): a ceiling on the tool call's wall time. It bounds the call, not the first-use image build. A run the daemon kills for exceeding it is reported as `exceeded max_duration_seconds`, not as a raw context error. A run that finished but ran long is failed against its recorded duration.

`max_cost_usd` and `max_duration_seconds` are passed into the run as its budget and timeout, so they both bound the run in flight and are re-checked against what it actually used. Negative values for either are rejected at load.

## The LLM judge

A case can add a judge to grade output no substring check can:

```yaml
    judge:
      enabled: true
      prompt: "Score 1.0 if the summary is accurate and one sentence, else lower."
      pass_threshold: 0.7
```

When `enabled` is true the case must have a non-empty `prompt`, and `pass_threshold` must be in the range (0, 1]. The judge sees the case's input (tool name plus its args as JSON) and the agent's output, scores it, and the case fails if the score is below the threshold.

The judge runs on your machine in the trusted CLI process, using your provider key. It never runs inside a cage, so agent code never touches that key. The grading call uses temperature 0 and asks the model for a bare JSON object `{"score": ..., "reason": ...}`. A reply it cannot parse into a number is retried once, then fails the case closed: a judge that cannot produce a score is never read as a pass. Each grading call is bounded at 90 seconds, so a wedged provider fails one case rather than the whole suite. A score below 0 or above 1 is clamped into range.

The judge's own token cost is reported in the summary footer but is never counted against a case's `max_cost_usd`. That ceiling measures the agent, not your choice to grade it.

### Which model judges

Without `--judge-model`, the judge uses your default LLM provider, and that provider must have a model configured. With `--judge-model provider/model`, it uses that provider instead, which must already be configured (a typo stops you rather than silently falling back to the default). Either way the provider needs a `key_ref` whose secret is in your store. A missing default provider, a default with no model, an unconfigured named provider, or a missing key each fail before any case runs, so the suite does not burn money and then die at the first judged case. `eval` only builds the judge when the selected run actually contains a judged case.

## Running one case

`--case NAME` runs a single case and skips the rest. An unknown name fails immediately and lists the cases the suite does declare. A single-case run does **not** record results into the manifest, since one case does not cover the suite and its counts would misrepresent the agent. Use it to iterate on one case without stamping a partial score.

## Recording results

A full-suite run (no `--case`) stamps its outcome into the bundle's manifest on disk: `declared`, the run timestamp, passed and failed counts, and the mean judge score across judged cases. The score then travels with the agent, so `mcpvessel inspect` shows it and anyone who pulls the bundle sees it. `eval` prints `Recorded results in the bundle manifest` after a full run. The counts are pointers in the manifest, so a suite that was declared but never run reads apart from one that ran and scored zero.

## Output

By default `eval` streams a line as each case starts and a result line as it finishes, with the agent's own logs and any build progress on stderr:

```
=== RUN   summarize_short
--- PASS: summarize_short (1.2s, $0.004210)

=== RUN   refuses_secret
--- FAIL: refuses_secret (0.8s, $0.001300)
    output contains forbidden "sk-"
```

At the end it prints one summary line. A clean run is tagged `ok`; any failure tags it `FAIL`. When any case ran a judge, a segment reports the mean score, how many cases were scored, and the judge's total cost:

```
ok	@okedeji/researcher:0.1  4 passed  judge 0.86 (2 scored, $0.001900)  $0.021400 in 6.3s
```

`--json` replaces all of that on stdout with a single machine-readable report (per-case results, aggregate counts, mean judge score, agent and judge spend in micro-USD, elapsed milliseconds). In JSON mode the live per-case lines are suppressed so stdout carries only the report; the agent's logs still go to stderr.

## Flags

| Flag | Meaning |
| --- | --- |
| `--case NAME` | Run only the named case. Does not record results. An unknown name errors and lists the cases the suite declares. |
| `--judge-model PROVIDER/MODEL` | Provider and model to grade judged cases. Must name a configured provider. Default: your default provider (which must have a model configured). |
| `--json` | Emit the machine-readable report on stdout and suppress the live per-case lines. |
| `--env KEY=VALUE` | Supply an env value to every case's run, or `KEY` alone to pass it through from your environment. Repeatable. |
| `--env-file PATH` | Read env values (`KEY=VALUE` per line) from a file. |
| `--secret NAME` | Supply a secret to every case's run, resolved from your environment or the mcpvessel secret store, never the command line. Repeatable. |
| `--secret-file PATH` | Read secret values (`NAME=VALUE` per line) from a permissions-restricted file. |

`--env` and `--secret` inputs are broadcast to every case in the run. Because `eval` runs one agent, there is nothing to scope a secret to, so scoped grants flatten to a plain pool.

## Examples

```sh
# Build the current directory and run its whole suite.
mcpvessel eval .

# Evaluate a tagged bundle from the store.
mcpvessel eval @okedeji/researcher:0.1

# Iterate on one case without recording a partial score.
mcpvessel eval @okedeji/researcher:0.1 --case summarize_short

# Grade judged cases with a specific configured provider.
mcpvessel eval . --judge-model openai/gpt-4o-mini

# Supply a tool credential the agent's cases need, and capture the report as JSON.
mcpvessel eval @okedeji/researcher:0.1 --secret BRAVE_API_KEY --json
```

## Notes

- The daemon must be up. `eval` preflights it once, so a stopped daemon is one clean error pointing you at `mcpvessel init`, not a connection-refused per case.
- Building a source directory for `eval` skips introspection and does not take your `--env` or `--secret`. Those inputs feed the cases when they run. If the source has changed in a way that affects its tool catalog, build it yourself first so the manifest is current, then `eval` the built bundle.
- `max_cost_usd` is a soft cap, not a hard cut-off. A case can finish slightly over and is then failed on the recorded cost. Set the ceiling with a little headroom for the cost of one in-flight call.
- The judge fails closed. An unreachable provider, a non-200 response, an empty completion, or a reply with no parseable score all fail the case rather than passing it.
- The judge's spend is separate from the agent's. It shows in the summary footer but never counts against a case's `max_cost_usd`.
- A run's exit code is non-zero whenever any case failed. In CI, a passing exit means every declared case passed.
- `mcpvessel push --with-evals` runs this same suite path and records the results into the manifest before pushing, so a published bundle carries a fresh score.

## See also

- [build](build.md): how the bundle `eval` reads is produced, and the `EVAL` directive that declares the suite.
- [run](run.md): the one-shot path each case takes, including how `--budget` and the run timeout behave.
- [call](call.md): invoking a single public tool by name, the contract a case's `input.tool` must satisfy.
- [inspect](inspect.md): where a recorded eval score is shown on a bundle.
- [push](push.md): `--with-evals` runs the suite and records results before publishing.
- [secrets](secrets.md), [config](config.md): storing the provider key and configuring the model the judge uses.
