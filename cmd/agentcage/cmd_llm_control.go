package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/env"
)

// newLLMControlCmd is the client the daemon execs inside an LLM gateway
// container to drive its control surface. It is hidden: the daemon runs it via
// nerdctl exec, never an operator. Running inside the container, it reaches the
// gateway's loopback control listener that nothing on the run network can.
func newLLMControlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "llm-control",
		Short:  "Drive the in-run LLM gateway control surface (internal)",
		Hidden: true,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "budget MICRO_USD",
		Short: "Set the gateway's budget in micro-USD",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			micro, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("budget %q is not micro-USD: %w", args[0], err)
			}
			body, err := json.Marshal(map[string]int64{"micro_usd": micro})
			if err != nil {
				return err
			}
			url := "http://127.0.0.1:" + env.DefaultLLMControlPort + "/budget"
			resp, err := http.Post(url, "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("reaching the gateway control listener: %w", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("gateway rejected the budget change: %s", resp.Status)
			}
			return nil
		},
	})
	return cmd
}
