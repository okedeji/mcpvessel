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
	cmd.AddCommand(newDaemonStopCmd())
	return cmd
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon, releasing its agents cleanly",
		Long: `Ask the running daemon to shut down.

It releases every running agent, and the containers and networks behind them,
before it exits, so nothing is orphaned. This is the supported alternative to
killing the process: a SIGKILL would leave a run's containers and per-run
network behind for the next startup to sweep. A no-op when nothing is running.

In production the process manager owns the daemon (systemd on Linux, launchd on
the macOS host) and stops it with SIGTERM, which runs this same clean shutdown.
Use this command for local development, where you started the daemon yourself.`,
		Example: `  agentcage daemon stop`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stopped, err := daemon.Stop(cmd.Context())
			if err != nil {
				return err
			}
			msg := "no daemon is running"
			if stopped {
				msg = "daemon stopped"
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}
}
