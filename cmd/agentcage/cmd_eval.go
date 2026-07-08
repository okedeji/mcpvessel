package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/eval"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/progress"
	"github.com/okedeji/agentcage/internal/secrets"
)

func newEvalCmd() *cobra.Command {
	var caseName, judgeModel, envFile, secretFile string
	var envFlags, secretFlags []string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "eval TARGET",
		Short: "Run an agent's eval suite",
		Long: `Run the eval suite an agent declares with EVAL and report the results.

TARGET is a reference ('agentcage build -t' put it in the store), the content
hash an untagged build printed, a path to a .agent file, or a source directory
('.') that eval builds first. Every case runs in a real cage the same way
'agentcage run' does, so LLM tokens cost real money.

A case passes when all of its declared expectations pass: output_contains and
output_not_contains substrings, a max_cost_usd ceiling, a max_duration_seconds
ceiling, and an optional LLM judge that scores the output against a rubric. A
case that errors, times out, or overspends is a failure, not an abort; the suite
runs to the end and eval exits non-zero if any case failed.

A full run records its results (passed, failed, judge score, timestamp) into the
bundle's manifest, so 'agentcage inspect' and anyone who pulls the agent can see
them. A single --case run does not record, since it does not cover the suite.

The judge, when a case enables one, uses your default LLM provider; --judge-model
provider/model picks another configured provider. The judge runs on your machine
with the provider key, never inside a cage.`,
		Example: `  agentcage eval .
  agentcage eval @okedeji/researcher:0.1
  agentcage eval @okedeji/researcher:0.1 --case summarize_short
  agentcage eval . --judge-model openai/gpt-4o-mini`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, bundlePath, display, err := resolveEvalTarget(cmd.Context(), cmd.ErrOrStderr(), args[0])
			if err != nil {
				return err
			}

			manifest, err := bundle.ReadManifest(bundlePath)
			if err != nil {
				return err
			}
			if manifest.Agentfile.Eval == "" {
				return fmt.Errorf("bundle %s declares no EVAL suite; add an EVAL directive and rebuild", display)
			}
			data, err := bundle.ReadSourceFile(bundlePath, manifest.Agentfile.Eval)
			if err != nil {
				return err
			}
			suite, err := eval.LoadSuite(data)
			if err != nil {
				return err
			}

			envPool, secretPool, err := buildInputPools(envFlags, envFile, secretFlags, secretFile)
			if err != nil {
				return err
			}

			// JSON mode: stdout carries only the machine report, so live
			// per-case lines are suppressed.
			results := cmd.OutOrStdout()
			if jsonOut {
				results = nil
			}
			report, err := runSuiteForBundle(cmd.Context(), suiteParams{
				ref:        ref,
				manifest:   manifest,
				suite:      suite,
				caseName:   caseName,
				judgeModel: judgeModel,
				env:        envPool,
				secrets:    secretPool,
				results:    results,
				logs:       cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			// Stamp full-suite runs only; a --case run's counts would
			// misrepresent the agent.
			if caseName == "" {
				if err := eval.Stamp(bundlePath, report, time.Now()); err != nil {
					return fmt.Errorf("recording eval results: %w", err)
				}
			}

			if jsonOut {
				b, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				printSummary(cmd.OutOrStdout(), display, report)
				if caseName == "" {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Recorded results in the bundle manifest")
				}
			}

			if report.Failed > 0 {
				return fmt.Errorf("%d of %d cases failed", report.Failed, report.Passed+report.Failed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&caseName, "case", "", "run only the named case (does not record results)")
	cmd.Flags().StringVar(&judgeModel, "judge-model", "", "provider/model to grade judged cases (default: your default provider)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the machine-readable report")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value: KEY=VALUE, or KEY to pass it through from your environment (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME, resolved from your environment or the agentcage secret store (repeatable)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read secret values (NAME=VALUE per line) from a perms-restricted file")
	return cmd
}

// suiteParams is shared by eval and push --with-evals.
type suiteParams struct {
	ref        string
	manifest   *bundle.Manifest
	suite      *eval.Suite
	caseName   string
	judgeModel string
	env        map[string]string
	secrets    map[string]string
	results    io.Writer // per-case PASS/FAIL lines; nil suppresses them (JSON mode)
	logs       io.Writer // build progress and agent stderr
}

// runSuiteForBundle preflights the daemon so a down daemon is one clean error,
// not a connection refused per case, and builds the judge only when a selected
// case needs one.
func runSuiteForBundle(ctx context.Context, p suiteParams) (*eval.Report, error) {
	socket, err := daemon.SocketPath()
	if err != nil {
		return nil, err
	}
	client := daemon.Dial(socket)
	if _, err := client.Version(ctx); err != nil {
		return nil, fmt.Errorf("cannot reach the agentcage daemon, run 'agentcage init' to start it: %w", err)
	}

	opts := eval.Options{
		Ref:      p.ref,
		Manifest: p.manifest,
		Suite:    p.suite,
		CaseName: p.caseName,
		Env:      p.env,
		Secrets:  p.secrets,
		Logs:     p.logs,
	}
	if p.results != nil {
		opts.OnStart = func(name string) { _, _ = fmt.Fprintf(p.results, "=== RUN   %s\n", name) }
		opts.OnCase = func(r eval.CaseResult) { printCaseResult(p.results, r) }
	}

	if suiteNeedsJudge(p.suite, p.caseName) {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		sec, err := secrets.Load()
		if err != nil {
			return nil, err
		}
		judge, err := eval.NewJudge(cfg, sec, p.judgeModel)
		if err != nil {
			return nil, err
		}
		return eval.Run(ctx, client, judge, opts)
	}
	return eval.Run(ctx, client, nil, opts)
}

// resolveEvalTarget turns the eval argument into a daemon-resolvable ref, the
// local bundle path to read and stamp, and a display name. A source directory
// is built into the store first: the suite must come from a packed bundle.
func resolveEvalTarget(ctx context.Context, stderr io.Writer, arg string) (ref, bundlePath, display string, err error) {
	if info, statErr := os.Stat(arg); statErr == nil && info.IsDir() {
		hash, storePath, buildErr := buildIntoStore(ctx, stderr, stderr, buildConfig{
			srcDir:       arg,
			mode:         progress.ModeAuto,
			noIntrospect: true,
		})
		if buildErr != nil {
			return "", "", "", buildErr
		}
		return hash, storePath, hash, nil
	}
	b, err := locate.Bundle(ctx, arg)
	if err != nil {
		return "", "", "", err
	}
	return arg, b.Path, b.Display, nil
}

func suiteNeedsJudge(s *eval.Suite, caseName string) bool {
	for _, c := range s.Cases {
		if caseName != "" && c.Name != caseName {
			continue
		}
		if c.HasJudge() {
			return true
		}
	}
	return false
}

func printCaseResult(w io.Writer, r eval.CaseResult) {
	status := "PASS"
	if !r.Passed {
		status = "FAIL"
	}
	_, _ = fmt.Fprintf(w, "--- %s: %s (%s, %s)\n", status, r.Name,
		r.Duration().Round(100*time.Millisecond), eval.FormatUSD(r.CostMicroUSD))
	for _, f := range r.Failures {
		_, _ = fmt.Fprintf(w, "    %s\n", f)
	}
	// Blank line between cases: the daemon's logs interleave with these.
	_, _ = fmt.Fprintln(w)
}

func printSummary(w io.Writer, display string, r *eval.Report) {
	tag := "ok"
	counts := fmt.Sprintf("%d passed", r.Passed)
	if r.Failed > 0 {
		tag = "FAIL"
		counts = fmt.Sprintf("%d passed, %d failed", r.Passed, r.Failed)
	}
	judgeSeg := ""
	if r.JudgeCount > 0 {
		judgeSeg = fmt.Sprintf("  judge %.2f (%d scored, %s)", *r.JudgeScore, r.JudgeCount, eval.FormatUSD(r.JudgeCostMicroUSD))
	}
	_, _ = fmt.Fprintf(w, "%s\t%s  %s%s  %s in %s\n", tag, display, counts, judgeSeg,
		eval.FormatUSD(r.CostMicroUSD), r.Elapsed().Round(100*time.Millisecond))
}
