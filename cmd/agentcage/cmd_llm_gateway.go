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
			gw := llmgateway.New(cfg, llmgateway.Hooks{
				Spend:   func(r llmgateway.SpendReport) { llmgateway.WriteSpendLine(os.Stdout, r) },
				Call:    func(e llmgateway.CallEvent) { llmgateway.WriteCallLine(os.Stdout, e) },
				Payload: func(r llmgateway.CallRecord) { llmgateway.WriteReplayLine(os.Stdout, r) },
			})

			// The control surface listens on the container's loopback only, so
			// agents on the run network cannot reach it; the daemon drives it via
			// nerdctl exec, which runs inside this container's namespace.
			control := &http.Server{Addr: "127.0.0.1:" + env.DefaultLLMControlPort, Handler: gw.Control()}
			go func() {
				if err := control.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					fmt.Fprintf(os.Stderr, "llm gateway control listener: %v\n", err)
				}
			}()

			srv := &http.Server{Addr: addr, Handler: gw.Handler()}
			return srv.ListenAndServe()
		},
	}
}
