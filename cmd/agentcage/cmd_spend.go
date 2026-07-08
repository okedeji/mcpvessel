package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/llmgateway"
)

// spendWatchInterval paces --watch; each refresh reads the run's gateway.
const spendWatchInterval = 2 * time.Second

func newSpendCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "spend RUN",
		Short: "Show a live run's LLM spend",
		Long: `Show what a running agent has spent on LLM calls so far.

spend reads the live run's gateway, so it answers only while the run is up; a
finished run's total cost is in 'agentcage ps'. -w/--watch refreshes the total in
place until the run ends or you interrupt it.

The run id is the one 'agentcage ps' lists.`,
		Example: `  agentcage spend researcher-7a1c4f2e9d3b
  agentcage spend -w researcher-7a1c4f2e9d3b`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			c := daemon.Dial(socket)
			if watch {
				return watchSpend(cmd.Context(), c, args[0], cmd.OutOrStdout())
			}
			report, err := c.Spend(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printSpend(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh the total in place until the run ends")
	return cmd
}

// watchSpend reprints the total in place until interrupted. The first failed
// read is the run going away, a clean stop rather than an error.
func watchSpend(ctx context.Context, c *daemon.Client, id string, w io.Writer) error {
	t := time.NewTicker(spendWatchInterval)
	defer t.Stop()
	for {
		report, err := c.Spend(ctx, id)
		if err != nil {
			_, _ = fmt.Fprintln(w)
			return nil
		}
		_, _ = fmt.Fprintf(w, "\r%s   ", spendTotalLine(report))
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(w)
			return nil
		case <-t.C:
		}
	}
}

func spendTotalLine(r llmgateway.SpendReport) string {
	if r.BudgetMicroUSD > 0 {
		return fmt.Sprintf("LLM spend: $%s of $%s budget", formatUSDMicros(r.TotalMicroUSD), formatUSDMicros(r.BudgetMicroUSD))
	}
	return fmt.Sprintf("LLM spend: $%s (no budget set)", formatUSDMicros(r.TotalMicroUSD))
}

// printSpend writes the total and a per-agent breakdown in stable order.
func printSpend(w io.Writer, r llmgateway.SpendReport) {
	_, _ = fmt.Fprintln(w, spendTotalLine(r))
	keys := make([]string, 0, len(r.Agents))
	for k := range r.Agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := r.Agents[k]
		unit := "calls"
		if a.Calls == 1 {
			unit = "call"
		}
		_, _ = fmt.Fprintf(w, "  %-12s $%s  (%d %s)\n", k, formatUSDMicros(a.SpentMicroUSD), a.Calls, unit)
	}
}
