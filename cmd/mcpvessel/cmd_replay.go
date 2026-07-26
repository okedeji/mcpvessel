package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/locate"
	"github.com/okedeji/mcpvessel/internal/replay"
)

func newReplayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Record a run for later replay",
		Long: `Record a run's full payloads into a .replay artifact.

The artifact captures every LLM call's request and response, so you can keep a
run, share it when reporting a bug, or analyze it yourself.`,
	}
	cmd.AddCommand(newReplayRecordCmd())
	return cmd
}

func newReplayRecordCmd() *cobra.Command {
	var envFlags, secretFlags, egressFlags []string
	var envFile, secretFile, budget string
	var inspectEgress bool
	cmd := &cobra.Command{
		Use:   "record BUNDLE [PROMPT]",
		Short: "Run an agent and record its full payloads to a .replay artifact",
		Long: `Run an agent like 'mcpvessel run', capturing every LLM call's full request and
response into ~/.mcpvessel/replays/<run-id>.replay.

The request bodies are the agent-facing bodies the gateway sees, captured before
it attaches the provider key, so a recording never contains a key.`,
		Example: `  mcpvessel replay record @okedeji/researcher:0.1 "summarize Q3 earnings"`,
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := locate.Bundle(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			manifest, err := bundle.ReadManifest(b.Path)
			if err != nil {
				return err
			}
			if manifest.Vesselfile.Main == "" {
				return fmt.Errorf("bundle %s has no MAIN; replay record runs an agent's MAIN, not a tool collection", b.Display)
			}
			toolArgs := map[string]any{}
			if len(args) > 1 && args[1] != "" {
				toolArgs["messages"] = []map[string]string{{"role": "user", "content": args[1]}}
			}

			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			var budgetMicros int64
			if budget != "" {
				m, err := parseUSDMicros(budget)
				if err != nil {
					return fmt.Errorf("--budget %q is not a USD amount", budget)
				}
				if m == 0 {
					return fmt.Errorf("--budget must be a positive amount; omit it to leave the run unbounded")
				}
				budgetMicros = m
			}
			// The same input pools run builds, so a recorded run is the run it
			// would have been: flags overlaid on config-bound secrets.
			envPool, secretPool, err := buildInputPools(envFlags, envFile, secretFlags, secretFile)
			if err != nil {
				return err
			}
			if err := applyConfigSecrets(secretPool, args[0], cmd.ErrOrStderr()); err != nil {
				return err
			}
			runID, result, err := daemon.Dial(socket).RecordRun(cmd.Context(), daemon.RunRequest{
				Ref:     args[0],
				Tool:    manifest.Vesselfile.Main,
				Args:    toolArgs,
				Budget:  budgetMicros,
				Env:     envPool,
				Secrets: secretPool,
				Inspect: inspectEgress,
				Egress:  egress.ParseScoped(egressFlags),
			}, cmd.ErrOrStderr())
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("cannot reach the mcpvessel daemon, run 'mcpvessel init' to start it: %w", err)
				}
				return err
			}

			if !strings.HasSuffix(result, "\n") {
				result += "\n"
			}
			_, _ = io.WriteString(cmd.OutOrStdout(), result)
			return saveReplay(cmd, socket, runID)
		},
	}
	cmd.Flags().StringVar(&budget, "budget", "", "cap the run's LLM spend in USD, e.g. 5.00 (overrides the agent's advisory BUDGET)")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value: KEY=VALUE, or KEY to pass it through from your environment (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME, or agent:NAME to grant one agent of several (repeatable)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read secret values ([agent:]NAME=VALUE per line) from a perms-restricted file")
	cmd.Flags().StringArrayVar(&egressFlags, "egress", nil, "allow the agent hosts for this run: host,host, or agent:host,host to scope one (repeatable)")
	cmd.Flags().BoolVar(&inspectEgress, "egress-inspect", false, "also decrypt each cage's outbound HTTPS to an approved host to record what it sent (opt-in)")
	return cmd
}

// saveReplay fetches the run's artifact from the daemon and writes a host copy.
func saveReplay(cmd *cobra.Command, socket, runID string) error {
	data, err := daemon.Dial(socket).FetchReplay(cmd.Context(), runID)
	if err != nil {
		return fmt.Errorf("the run finished but its replay could not be fetched: %w", err)
	}
	path, err := replay.Path(runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing replay artifact: %w", err)
	}

	events := 0
	var rec replay.Recording
	if json.Unmarshal(data, &rec) == nil {
		events = len(rec.Events)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Recorded %d event(s) to %s\n", events, path)
	return nil
}
