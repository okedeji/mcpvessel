package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
)

func newPsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List running agents",
		Long: `List the agents the daemon is currently running.

ps talks to the daemon, so it needs one running. Each row is a run: its id,
the agent reference, its status, and how long it has been up.`,
		Example: `  agentcage ps`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			runs, err := daemon.Dial(socket).ListRuns(cmd.Context())
			if err != nil {
				return fmt.Errorf("%w (is the daemon running? start it with 'agentcage daemon')", err)
			}
			printRuns(cmd.OutOrStdout(), runs)
			return nil
		},
	}
	return cmd
}

// printRuns renders the ps table. The header prints even when there are no
// runs, so an empty list reads as "nothing running" rather than blank output.
func printRuns(w io.Writer, runs []daemon.RunInfo) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RUN ID\tREF\tSTATUS\tUP")
	for _, r := range runs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Ref, r.Status, since(r.StartedAt))
	}
	_ = tw.Flush()
}

// since formats how long a run has been up as a single coarse unit ("3s", "5m",
// "2h"), reading the clock through nowFunc so tests stay deterministic.
func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := nowFunc().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// nowFunc is overridable so since() is testable without a real clock.
var nowFunc = time.Now
