package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/identity"
	"github.com/okedeji/agentcage/internal/wrap"
)

// newMCPBridgeCmd builds the ENTRYPOINT for imported stdio-only MCP servers.
// With AGENTCAGE_SERVE_HTTP set (sub-agent) it serves streamable HTTP and
// forwards every tool to the inner stdio server it spawns; unset (root) it
// execs the inner server so the bridge never sits in the stdio path the
// daemon drives. One static binary, so it works in any base image.
func newMCPBridgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    wrap.BridgeSubcommand + " -- SERVER [ARG...]",
		Short:  "Serve an imported stdio MCP server over HTTP so it can be a USES sub-agent",
		Hidden: true,
		// Everything after `--` is the inner server's command, forwarded verbatim.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			inner, err := innerServerCommand(args)
			if err != nil {
				return err
			}
			bind := os.Getenv(env.ServeHTTP)
			if bind == "" {
				return execInner(inner)
			}
			return serveBridge(cmd.Context(), bind, inner)
		},
	}
}

// innerServerCommand returns the tokens after `--`.
func innerServerCommand(args []string) ([]string, error) {
	for i, a := range args {
		if a == "--" {
			if i+1 >= len(args) {
				break
			}
			return args[i+1:], nil
		}
	}
	return nil, fmt.Errorf("mcp-bridge: expected '-- <server command>'")
}

// execInner replaces the bridge process with the inner server, handing it
// stdin and stdout wholesale; the daemon drives a root cage over stdio.
func execInner(inner []string) error {
	path, err := exec.LookPath(inner[0])
	if err != nil {
		return fmt.Errorf("mcp-bridge: locating %s: %w", inner[0], err)
	}
	if err := syscall.Exec(path, inner, os.Environ()); err != nil {
		return fmt.Errorf("mcp-bridge: exec %s: %w", inner[0], err)
	}
	return nil
}

// serveBridge spawns the inner stdio server and mirrors its tools onto an
// HTTP MCP server on bind, forwarding every tools/call unchanged.
func serveBridge(ctx context.Context, bind string, inner []string) error {
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: identity.Name, Version: identity.Version}, nil)
	proc := exec.Command(inner[0], inner[1:]...)
	proc.Stderr = os.Stderr
	session, err := client.Connect(ctx, &mcpsdk.CommandTransport{Command: proc}, nil)
	if err != nil {
		return fmt.Errorf("mcp-bridge: connecting to %s: %w", inner[0], err)
	}
	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcp-bridge: listing tools from %s: %w", inner[0], err)
	}

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: identity.Name, Version: identity.Version}, nil)
	for _, t := range tools.Tools {
		name := t.Name
		server.AddTool(
			&mcpsdk.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema},
			func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: req.Params.Arguments})
			},
		)
	}

	// The per-run gateway forwards its own Host header, which the SDK's
	// DNS-rebinding guard would 403. Only the private run network can reach
	// this port, so the guard is safe to disable.
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return server },
		&mcpsdk.StreamableHTTPOptions{DisableLocalhostProtection: true},
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/mcp/", handler)
	return (&http.Server{Addr: bind, Handler: mux}).ListenAndServe()
}
