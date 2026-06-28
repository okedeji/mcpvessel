package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
)

func newLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs RUN",
		Short: "Show a run's logs",
		Long: `Show the logs of a run, live or finished.

logs reads the run's durable log through the daemon, so it works after the run
has ended and after a daemon restart. -f follows a live run, streaming new output
until the run ends.

The run id is the one 'agentcage ps' lists.`,
		Example: `  agentcage logs researcher-7a1c4f2e9d3b
  agentcage logs -f researcher-7a1c4f2e9d3b`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			if err := daemon.Dial(socket).Logs(cmd.Context(), args[0], follow, cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("%w (is the daemon running? start it with 'agentcage daemon')", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new output until the run ends")
	return cmd
}
