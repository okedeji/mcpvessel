package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
)

func newServeCmd() *cobra.Command {
	var listen string
	var expose, noExpose []string
	cmd := &cobra.Command{
		Use:   "serve BUNDLE",
		Short: "Serve an agent to external MCP clients over HTTP",
		Long: `Serve an agent to external MCP clients over HTTP.

BUNDLE is a reference (resolved store-first, then pulled), a content hash from an
untagged build, or a path to a .agent file, the same as 'agentcage run'.

serve opens one MCP endpoint per exposed agent under /agents/. The agent you name
is exposed; so is any USES PUBLIC sub-agent in its tree. Transitive private
sub-agents stay unreachable. --expose and --no-expose override per agent, matched
by repository, and --no-expose wins.

serve talks to the daemon, so it needs one running. It returns once the front
door is open; the daemon keeps serving until you 'agentcage stop' the runs or it
shuts down.`,
		Example: `  agentcage serve --listen :7000 @me/researcher:0.1
  agentcage serve --listen 127.0.0.1:7000 --no-expose @me/creddb @me/researcher:0.1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			res, err := daemon.Dial(socket).Serve(cmd.Context(), args[0], listen, expose, noExpose)
			if err != nil {
				return fmt.Errorf("%w (is the daemon running?)", err)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Serving %d agent(s) on %s\n", len(res.Agents), res.Listen)
			for _, a := range res.Agents {
				_, _ = fmt.Fprintf(out, "  /agents/%s/mcp", a.Address)
				if len(a.Tools) > 0 {
					_, _ = fmt.Fprintf(out, "  tools: %s", strings.Join(a.Tools, ", "))
				}
				_, _ = fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address to bind the MCP front door to, e.g. :7000 (required)")
	cmd.Flags().StringArrayVar(&expose, "expose", nil, "also expose this agent, matched by repository (repeatable)")
	cmd.Flags().StringArrayVar(&noExpose, "no-expose", nil, "hide this agent even if USES PUBLIC, matched by repository (repeatable)")
	_ = cmd.MarkFlagRequired("listen")
	return cmd
}
