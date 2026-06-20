package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
)

func newBudgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "budget",
		Short: "Manage a running agent's LLM budget",
	}
	cmd.AddCommand(newBudgetSetCmd())
	return cmd
}

func newBudgetSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set RUN AMOUNT",
		Short: "Change a running agent's LLM budget mid-run",
		Long: `Change a running agent's LLM budget without restarting it.

RUN is the run id from 'agentcage ps'. AMOUNT is USD, e.g. 5.00. Raising the
budget lets a run that hit its cap continue; lowering it stops the next call.
An in-flight call is not aborted. Only runs that reason (and so have an LLM
gateway) have a budget to set.`,
		Example: `  agentcage budget set researcher-7a1c4f2e9d3b 10.00`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			micro, err := parseUSDMicros(args[1])
			if err != nil {
				return fmt.Errorf("%q is not a USD amount", args[1])
			}
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			if err := daemon.Dial(socket).SetBudget(cmd.Context(), args[0], micro); err != nil {
				return fmt.Errorf("%w (is the daemon running?)", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "budget for %s set to $%s\n", args[0], args[1])
			return nil
		},
	}
	return cmd
}
