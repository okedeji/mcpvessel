package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/progress"
)

func newEventsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream daemon lifecycle events",
		Long: `Stream a live feed of daemon events as they happen: runs starting and ending,
with each run's final status.

events stays connected and prints each event until you interrupt it. In a
terminal it prints a readable line per event; piped or redirected it prints one
JSON object per line. --json forces JSON even at a terminal, for an agent
watching the feed.`,
		Example: `  mcpvessel events`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			// The feed has no backlog, so a quiet start looks hung; tell the
			// human it is listening. Piped or --json output stays pure JSON lines.
			if !jsonOut && progress.IsTerminal(cmd.OutOrStdout()) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Listening for daemon events (Ctrl-C to stop)")
			}
			emit := eventPrinter(cmd.OutOrStdout(), jsonOut)
			if err := daemon.Dial(socket).Events(cmd.Context(), emit); err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("%w (the daemon is not running; start it with 'mcpvessel init')", err)
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON, one object per line, even at a terminal")
	return cmd
}

// eventPrinter picks readable lines for a terminal, JSON lines for a pipe or
// when forceJSON is set, the same split the rest of the observability output
// uses.
func eventPrinter(w io.Writer, forceJSON bool) func(daemon.Event) {
	if forceJSON || !progress.IsTerminal(w) {
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
