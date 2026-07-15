package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/daemon"
)

func newStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop RUN...",
		Short: "Stop running agents",
		Long: `Stop running agents and release their containers and networks.

Each RUN is a run id 'mcpvessel ps' lists. stop talks to the daemon, so it
needs one running.`,
		Example: `  mcpvessel stop researcher-7a1c4f2e9d3b
  mcpvessel stop researcher-7a1c4f2e9d3b oncall-2b8d11c04e7f`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			client := daemon.Dial(socket)
			return stopRuns(cmd.Context(), client.StopRun, cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}
	return cmd
}

// stopRuns stops each run in order, continuing past a failure so one bad id
// does not leave the rest running; the summary error carries the non-zero
// exit. An unreachable daemon aborts instead: every remaining call would
// fail the same way, and the hint matters more than the tally.
func stopRuns(ctx context.Context, stop func(context.Context, string) error, stdout, stderr io.Writer, ids []string) error {
	failed := 0
	for _, id := range ids {
		if err := stop(ctx, id); err != nil {
			var unreachable *daemon.Unreachable
			if errors.As(err, &unreachable) {
				return fmt.Errorf("%w (is the daemon running? start it with 'mcpvessel init')", err)
			}
			failed++
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", id, err)
			continue
		}
		_, _ = fmt.Fprintln(stdout, id)
	}
	if failed > 0 {
		return fmt.Errorf("failed to stop %d of %d run(s)", failed, len(ids))
	}
	return nil
}
