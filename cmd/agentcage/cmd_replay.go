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

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/replay"
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
	cmd := &cobra.Command{
		Use:   "record BUNDLE [PROMPT]",
		Short: "Run an agent and record its full payloads to a .replay artifact",
		Long: `Run an agent like 'agentcage run', capturing every LLM call's full request and
response into ~/.agentcage/replays/<run-id>.replay.

The request bodies are the agent-facing bodies the gateway sees, captured before
it attaches the provider key, so a recording never contains a key.`,
		Example: `  agentcage replay record @okedeji/researcher:0.1 "summarize Q3 earnings"`,
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
			if manifest.Agentfile.Main == "" {
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
			runID, result, err := daemon.Dial(socket).RecordRun(cmd.Context(), daemon.RunRequest{
				Ref:  args[0],
				Tool: manifest.Agentfile.Main,
				Args: toolArgs,
			}, cmd.ErrOrStderr())
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("cannot reach the agentcage daemon, run 'agentcage init' to start it: %w", err)
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
	return cmd
}

// saveReplay fetches the run's artifact from the daemon and writes a host copy,
// then prints a one-line confirmation with the path and event count.
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
