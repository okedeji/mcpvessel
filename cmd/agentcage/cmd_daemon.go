package main

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   daemon.Command,
		Short: "Run the agentcage control-plane daemon",
		Long: `Run the long-lived daemon that supervises agents and answers the CLI.

The daemon listens on a Unix socket under ~/.agentcage and tracks running
agents so 'agentcage ps', 'logs', and 'stop' can reach them. It runs where
containerd does: on Linux that is this host, so you start it directly (under
systemd in production). On macOS the host starts it inside the hidden Linux VM
for you, so you do not run this command yourself there.

It runs in the foreground and shuts down cleanly on SIGINT or SIGTERM.`,
		Example: `  agentcage daemon`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentcage daemon listening on %s\n", socket)
			return daemon.Serve(ctx, daemon.New(), socket)
		},
	}
	return cmd
}
