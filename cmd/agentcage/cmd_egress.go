package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/egress"
	"github.com/okedeji/agentcage/internal/env"
)

// newEgressCmd runs the in-run egress proxy. Hidden: the runtime starts it
// inside the egress container; its per-source allow lists arrive as injected
// environment.
func newEgressCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "egress",
		Short:  "Run the in-run egress proxy (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			raw := os.Getenv(env.EgressConfig)
			if raw == "" {
				return fmt.Errorf("%s is required", env.EgressConfig)
			}
			var cfg egress.Config
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				return fmt.Errorf("parsing %s: %w", env.EgressConfig, err)
			}
			addr := os.Getenv(env.EgressAddr)
			if addr == "" {
				addr = ":" + env.DefaultEgressPort
			}
			srv := &http.Server{Addr: addr, Handler: egress.Handler(cfg)}
			return srv.ListenAndServe()
		},
	}
}
