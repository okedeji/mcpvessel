package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/mcpgateway"
)

// newMCPGatewayCmd runs the in-run MCP gateway. Hidden: the runtime starts it
// inside the gateway container, with routing table and address in the environment.
func newMCPGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "mcp-gateway",
		Short:  "Run the in-run MCP gateway (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := mcpGatewayConfigFromEnv()
			if err != nil {
				return err
			}
			addr := os.Getenv(env.MCPAddr)
			if addr == "" {
				addr = ":" + env.DefaultMCPGatewayPort
			}
			gw := mcpgateway.New(cfg)
			gw.SetHooks(mcpgateway.Hooks{
				Call:    func(e mcpgateway.SubCallEvent) { mcpgateway.WriteSubCallLine(os.Stdout, e) },
				Payload: func(r mcpgateway.SubCallRecord) { mcpgateway.WriteSubReplayLine(os.Stdout, r) },
			})

			// The control stream listens on container loopback only, out of
			// reach of the run network; the daemon drives it by exec'ing the
			// mcp-control bridge into this container.
			go serveMCPControl(gw)

			srv := &http.Server{Addr: addr, Handler: gw.Handler()}
			return srv.ListenAndServe()
		},
	}
	return cmd
}

// serveMCPControl accepts one control connection at a time; the daemon holds a
// single bridge per run and re-execs a dead one, so loop back to Accept.
func serveMCPControl(gw *mcpgateway.Gateway) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+env.DefaultMCPControlPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp gateway control listener: %v\n", err)
		return
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp gateway control accept: %v\n", err)
			return
		}
		_ = gw.ServeControl(conn)
	}
}

// mcpGatewayConfigFromEnv reads the routing table the runtime injected.
func mcpGatewayConfigFromEnv() (mcpgateway.Config, error) {
	raw := os.Getenv(env.MCPConfig)
	if raw == "" {
		return mcpgateway.Config{}, fmt.Errorf("%s is required", env.MCPConfig)
	}
	var cfg mcpgateway.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return mcpgateway.Config{}, fmt.Errorf("parsing %s: %w", env.MCPConfig, err)
	}
	return cfg, nil
}
