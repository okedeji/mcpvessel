package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/progress"
)

func newEventsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream daemon lifecycle events",
		Long: `Stream a live feed of daemon events as they happen: runs starting and ending,
with each run's final status.

events stays connected and prints each event until you interrupt it. In a
terminal it prints a readable line per event; piped or redirected it prints one
JSON object per line.`,
		Example: `  agentcage events`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			emit := eventPrinter(cmd.OutOrStdout())
			if err := daemon.Dial(socket).Events(cmd.Context(), emit); err != nil {
				return fmt.Errorf("%w (is the daemon running? start it with 'agentcage daemon')", err)
			}
			return nil
		},
	}
	return cmd
}

// eventPrinter picks readable lines for a terminal, JSON lines for a pipe,
// the same split the rest of the observability output uses.
func eventPrinter(w io.Writer) func(daemon.Event) {
	if !progress.IsTerminal(w) {
		enc := json.NewEncoder(w)
		return func(e daemon.Event) { _ = enc.Encode(e) }
	}
	return func(e daemon.Event) { printEvent(w, e) }
}

func printEvent(w io.Writer, e daemon.Event) {
	// run.started/ended: label is the status, subject is the run. Runtime
	// events (cage/elicitation): label is the type, subject the sub-agent.
	label, subject := e.Type, e.RunID
	switch e.Type {
	case daemon.EventRunStarted:
		label = "started"
	case daemon.EventRunEnded:
		label = e.Status
	default:
		if e.Target != "" {
			subject = e.RunID + "/" + e.Target
		}
	}
	line := fmt.Sprintf("%s  %-20s %s", e.Time.Format("15:04:05"), label, subject)
	if e.Type == daemon.EventRunStarted || e.Type == daemon.EventRunEnded {
		if e.Ref != "" {
			line += "  " + e.Ref
		}
	}
	if e.Detail != "" {
		line += "  " + e.Detail
	}
	_, _ = fmt.Fprintln(w, line)
}
