package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/runtime"
)

// statsWatchInterval paces --watch refreshes.
const statsWatchInterval = 2 * time.Second

func newStatsCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show live per-cage resource usage",
		Long: `Show a live snapshot of every running cage's CPU, memory, and pid count.

A cage is one container in a run: the agent, its gateways, and any sub-agents.
-w/--watch refreshes the table in place until you interrupt it.`,
		Example: `  agentcage stats
  agentcage stats -w`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			c := daemon.Dial(socket)
			if watch {
				return watchStats(cmd.Context(), c, cmd.OutOrStdout())
			}
			stats, err := c.Stats(cmd.Context())
			if err != nil {
				return fmt.Errorf("%w (is the daemon running? start it with 'agentcage daemon')", err)
			}
			printStats(cmd.OutOrStdout(), stats)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh the table until interrupted")
	return cmd
}

func watchStats(ctx context.Context, c *daemon.Client, w io.Writer) error {
	t := time.NewTicker(statsWatchInterval)
	defer t.Stop()
	for {
		stats, err := c.Stats(ctx)
		if err != nil {
			return fmt.Errorf("%w (is the daemon running?)", err)
		}
		// Clear the screen and home the cursor.
		_, _ = io.WriteString(w, "\033[2J\033[H")
		printStats(w, stats)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func printStats(w io.Writer, stats []runtime.CageStat) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CAGE\tCPU\tMEM\tPIDS")
	for _, s := range stats {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, dash(s.CPU), dash(s.Mem), dash(s.PIDs))
	}
	_ = tw.Flush()
}

// dash marks a value nerdctl did not report.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
