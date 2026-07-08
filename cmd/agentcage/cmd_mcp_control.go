package main

import (
	"fmt"
	"io"
	"net"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/env"
)

// newMCPControlCmd bridges the daemon's activation stream into the MCP gateway
// container. Hidden: the daemon exec's it via nerdctl, never an operator. It
// dials the gateway's loopback-only control listener, unreachable from the run
// network, and copies its stdin/stdout straight through.
func newMCPControlCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp-control",
		Short:  "Bridge the daemon's MCP gateway activation stream (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := net.Dial("tcp", "127.0.0.1:"+env.DefaultMCPControlPort)
			if err != nil {
				return fmt.Errorf("reaching the gateway control listener: %w", err)
			}
			defer func() { _ = conn.Close() }()

			// Either direction closing ends the bridge; the daemon re-execs it.
			errc := make(chan error, 2)
			go func() {
				_, err := io.Copy(conn, os.Stdin)
				errc <- err
			}()
			go func() {
				_, err := io.Copy(os.Stdout, conn)
				errc <- err
			}()
			return <-errc
		},
	}
}
