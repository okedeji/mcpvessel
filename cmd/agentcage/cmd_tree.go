package main

import (
	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/runtime"
)

func newTreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tree BUNDLE|REF",
		Short: "Show the transitive USES tree of an agent",
		Long: `Print the full transitive USES tree of an agent: every sub-agent it pulls,
and every sub-agent those pull, down to the leaves.

The argument is either a local .agent file or a registry reference. Each
dependency is pulled by its locked digest (cache-first), the same way a run
resolves the tree, so the output is exactly what would run.

This is the audit surface for BAN: it shows every agent that will execute, by
the @org/name you would write into a 'BAN' directive to forbid one anywhere in
the subtree.`,
		Example: `  agentcage tree researcher.agent
  agentcage tree @okedeji/researcher:0.1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundlePath, display, err := resolveInspectTarget(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return runtime.PrintTree(cmd.Context(), bundlePath, display, cmd.OutOrStdout())
		},
	}
	return cmd
}
