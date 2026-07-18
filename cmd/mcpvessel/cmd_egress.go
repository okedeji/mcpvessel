package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/env"
)

// newEgressProxyCmd runs the in-run egress proxy. Hidden: the runtime starts it
// inside the egress container; its per-source allow lists arrive as injected
// environment.
func newEgressProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "egress-proxy",
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
			proxy := egress.New(cfg, os.Stdout)

			// Loopback only: cages on the run network cannot reach the control
			// surface; the daemon drives it via nerdctl exec inside this
			// container's namespace, mirroring the LLM gateway.
			control := &http.Server{Addr: "127.0.0.1:" + env.DefaultEgressControlPort, Handler: proxy.Control()}
			go func() {
				if err := control.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					fmt.Fprintf(os.Stderr, "egress control listener: %v\n", err)
				}
			}()

			srv := &http.Server{Addr: addr, Handler: proxy.Handler()}
			return srv.ListenAndServe()
		},
	}
}

// newEgressControlCmd is the internal client the daemon execs inside the egress
// proxy container to approve or reject a held host, reaching the proxy's
// loopback control surface. Mirrors llm-control.
func newEgressControlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "egress-control",
		Short:  "Drive the in-run egress proxy's control surface (internal)",
		Hidden: true,
	}
	post := func(path string) *cobra.Command {
		return &cobra.Command{
			Use:  path[1:] + " HOST",
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				url := "http://127.0.0.1:" + env.DefaultEgressControlPort + path + "?host=" + args[0]
				resp, err := http.Post(url, "", nil)
				if err != nil {
					return err
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode >= 300 {
					return fmt.Errorf("egress control %s: %s", path, resp.Status)
				}
				return nil
			},
		}
	}
	cmd.AddCommand(post("/allow"), post("/deny"))
	return cmd
}
