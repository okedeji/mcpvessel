package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/cliout"
	"github.com/okedeji/mcpvessel/internal/daemon"
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

The run id is the one 'mcpvessel ps' lists.`,
		Example: `  mcpvessel logs researcher-7a1c4f2e9d3b
  mcpvessel logs -f researcher-7a1c4f2e9d3b`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			// A quiet server writes no stderr and triggers no egress event, so
			// its log is legitimately empty; say so instead of printing nothing.
			// Follow mode streams until the run ends, so its silence is live.
			out := &countingWriter{w: cmd.OutOrStdout()}
			if err := daemon.Dial(socket).Logs(cmd.Context(), args[0], follow, out); err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("%w (the daemon is not running; start it with 'mcpvessel init')", err)
				}
				return err
			}
			if out.n == 0 && !follow {
				cliout.Empty(cmd.OutOrStdout(), "The run recorded no output: nothing on its stderr and no egress events.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new output until the run ends")
	return cmd
}

// countingWriter counts bytes so an empty log is distinguishable from a
// written one after the stream closes.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
