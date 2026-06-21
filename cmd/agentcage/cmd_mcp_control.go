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
// container. It is hidden: the daemon exec's it via nerdctl, never an operator.
// Running inside the container, it dials the gateway's loopback control listener
// that nothing on the run network can reach, then copies the daemon's stream
// (its stdin/stdout) straight through. It carries no protocol of its own; the
// activation logic lives in the gateway and the daemon, and this is the wire
// between them, the same way the LLM gateway's control client reaches its
// loopback listener.
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

			// Daemon -> gateway on one goroutine, gateway -> daemon on this one.
			// Either direction closing ends the bridge so the daemon re-execs it.
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
