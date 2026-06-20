package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/runtime"
)

func newCallCmd() *cobra.Command {
	var argPairs []string
	var verbose bool
	cmd := &cobra.Command{
		Use:   "call BUNDLE TOOL",
		Short: "Call a specific tool on an agent by name",
		Long: `Call a specific tool on an agent or tool collection by name.

Unlike 'agentcage run' (which routes a prompt to the bundle's MAIN
tool), 'agentcage call' invokes the tool the operator names. What
that tool does (reason with an LLM, call sub-agents, just fetch
data, or anything else) is whatever its author wrote. The platform
just routes the call.

BUNDLE is a reference (resolved store-first, then pulled), a content hash from
an untagged build, or a path to a .agent file, the same as 'agentcage run'.

Use 'call' when:

  - The bundle is a tool collection (no MAIN declared).
  - You want to address a specific exposed tool on an agent without
    going through its MAIN.

A tool is callable from outside the cage when the Agentfile declares
it via EXPOSE. The MAIN tool is implicitly public. Any other tool the
agent exposes via MCP is private and not reachable through 'call'.

Examples:

  agentcage call web-search.agent search --arg query="agentic memory"
  agentcage call researcher.agent fetch_paper --arg doi=10.1234/x.2026
  agentcage call github-mcp.agent create_pr --arg title="..." --arg body="..."`,
		Example: `  agentcage call @okedeji/web-search:0.1 search --arg query="agentic memory"
  agentcage call researcher.agent fetch_paper --arg doi=10.1234/x.2026`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := locate.Bundle(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			toolName := args[1]

			manifest, err := bundle.ReadManifest(b.Path)
			if err != nil {
				return err
			}
			if err := assertToolIsPublic(manifest, toolName); err != nil {
				return err
			}

			toolArgs, err := parseArgPairs(argPairs)
			if err != nil {
				return err
			}
			return runtime.Run(cmd.Context(), runtime.RunInput{
				BundlePath: b.Path,
				Name:       b.Name,
				Tool:       toolName,
				Args:       toolArgs,
				Stdout:     cmd.OutOrStdout(),
				Stderr:     cmd.ErrOrStderr(),
				Verbose:    verbose,
			})
		},
	}
	cmd.Flags().StringArrayVar(&argPairs, "arg", nil, "tool argument as KEY=VALUE (repeatable)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "stream the underlying provisioner output during first-time setup")
	return cmd
}

// parseArgPairs turns the repeated --arg KEY=VALUE flag into the map
// the MCP CallTool API expects. Returns an error naming the offending
// pair when one is malformed; the operator must see exactly which
// flag they got wrong.
func parseArgPairs(pairs []string) (map[string]any, error) {
	out := make(map[string]any, len(pairs))
	for _, p := range pairs {
		idx := strings.Index(p, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("--arg %q is not in KEY=VALUE form", p)
		}
		key := strings.TrimSpace(p[:idx])
		if key == "" {
			return nil, fmt.Errorf("--arg %q has an empty key", p)
		}
		value := p[idx+1:]
		out[key] = value
	}
	return out, nil
}

// assertToolIsPublic rejects external calls to private tools so the
// operator sees a clear error before the platform tries to spin up
// the cage. The Agentfile is the contract: a tool is public when it
// is the bundle's MAIN or appears in EXPOSE.
func assertToolIsPublic(manifest *bundle.Manifest, toolName string) error {
	if manifest.Agentfile.Main == toolName {
		return nil
	}
	for _, name := range manifest.Agentfile.Expose {
		if name == toolName {
			return nil
		}
	}
	publicNames := make([]string, 0, len(manifest.Agentfile.Expose)+1)
	if manifest.Agentfile.Main != "" {
		publicNames = append(publicNames, manifest.Agentfile.Main)
	}
	publicNames = append(publicNames, manifest.Agentfile.Expose...)
	if len(publicNames) == 0 {
		return fmt.Errorf("bundle exposes no public tools (no MAIN declared and EXPOSE list is empty)")
	}
	return fmt.Errorf("tool %q is not public on this bundle (public tools: %s)", toolName, strings.Join(publicNames, ", "))
}
