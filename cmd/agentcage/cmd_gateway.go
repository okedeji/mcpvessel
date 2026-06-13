package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/gateway"
)

// newGatewayCmd runs the in-run MCP gateway. It is hidden: the runtime
// starts it inside the gateway container, not operators. Its routing table
// and listen address arrive as environment the runtime injects.
func newGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "gateway",
		Short:  "Run the in-run MCP gateway (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := gatewayConfigFromEnv()
			if err != nil {
				return err
			}
			addr := os.Getenv(env.GatewayAddr)
			if addr == "" {
				addr = ":" + env.DefaultGatewayPort
			}
			srv := &http.Server{Addr: addr, Handler: gateway.Handler(cfg)}
			return srv.ListenAndServe()
		},
	}
	return cmd
}

// gatewayConfigFromEnv reads the routing table the runtime injected.
func gatewayConfigFromEnv() (gateway.Config, error) {
	raw := os.Getenv(env.GatewayConfig)
	if raw == "" {
		return gateway.Config{}, fmt.Errorf("%s is required", env.GatewayConfig)
	}
	var cfg gateway.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return gateway.Config{}, fmt.Errorf("parsing AGENTCAGE_GATEWAY_CONFIG: %w", err)
	}
	return cfg, nil
}
