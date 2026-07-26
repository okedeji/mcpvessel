package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		var all bool
		var agent string
		c := &cobra.Command{
			Use:  path[1:] + " HOST [SRC]",
			Args: cobra.RangeArgs(1, 2),
			RunE: func(cmd *cobra.Command, args []string) error {
				// Escape every value: the host is caged-server-supplied and the agent
				// name flows from the operator, and neither must be able to inject
				// extra query parameters or a fragment into the control URL.
				endpoint := "http://127.0.0.1:" + env.DefaultEgressControlPort + path +
					"?host=" + url.QueryEscape(args[0])
				if len(args) > 1 {
					endpoint += "&src=" + url.QueryEscape(args[1])
				}
				if agent != "" {
					endpoint += "&agent=" + url.QueryEscape(agent)
				}
				if all {
					endpoint += "&all=true"
				}
				resp, err := http.Post(endpoint, "", nil)
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
		c.Flags().BoolVar(&all, "all", false, "grant the host to every cage in the run")
		c.Flags().StringVar(&agent, "agent", "", "scope the decision to one agent by name")
		return c
	}
	cmd.AddCommand(post("/allow"), post("/deny"), egressCapturesCmd(), egressPreviewCtlCmd())
	return cmd
}

// egressPreviewCtlCmd GETs the full not-yet-approved request a cage wants to
// send a host and writes the JSON to stdout, for the daemon to read via nerdctl
// exec. Mirrors the captures client; loopback only.
func egressPreviewCtlCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "preview HOST",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint := "http://127.0.0.1:" + env.DefaultEgressControlPort + "/preview?host=" + url.QueryEscape(args[0])
			resp, err := http.Get(endpoint)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode >= 300 {
				return fmt.Errorf("egress control /preview: %s", resp.Status)
			}
			_, err = io.Copy(cmd.OutOrStdout(), resp.Body)
			return err
		},
	}
}

// egressCapturesCmd GETs the proxy's buffered inspection records and writes the
// JSON to stdout, for the daemon to read via nerdctl exec at teardown. Mirrors
// the allow/deny control clients; loopback only.
func egressCapturesCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "captures",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint := "http://127.0.0.1:" + env.DefaultEgressControlPort + "/captures"
			resp, err := http.Get(endpoint)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode >= 300 {
				return fmt.Errorf("egress control /captures: %s", resp.Status)
			}
			_, err = io.Copy(cmd.OutOrStdout(), resp.Body)
			return err
		},
	}
}
