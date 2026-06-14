package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/llmgateway"
)

// newLLMGatewayCmd runs the in-run LLM gateway. It is hidden: the runtime
// starts it inside the gateway container, not operators. Its endpoint set,
// per-agent models, and budget arrive as environment the runtime injects.
func newLLMGatewayCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "llm-gateway",
		Short:  "Run the in-run LLM gateway (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			raw := os.Getenv(env.LLMConfig)
			if raw == "" {
				return fmt.Errorf("%s is required", env.LLMConfig)
			}
			var cfg llmgateway.Config
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				return fmt.Errorf("parsing %s: %w", env.LLMConfig, err)
			}
			addr := os.Getenv(env.LLMAddr)
			if addr == "" {
				addr = ":" + env.DefaultLLMGatewayPort
			}
			report := func(r llmgateway.SpendReport) { llmgateway.WriteSpendLine(os.Stdout, r) }
			srv := &http.Server{Addr: addr, Handler: llmgateway.Handler(cfg, report)}
			return srv.ListenAndServe()
		},
	}
}
