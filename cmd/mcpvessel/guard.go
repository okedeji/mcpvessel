package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// strictApprovalEnv, set to "1", restores the stricter posture where the
// cage-widening commands (approving an egress host, binding a secret, persisting
// an allow-list) refuse to run without an interactive terminal, so no agent can
// perform them at all.
//
// It is off by default: an agent driving mcpvessel may run these commands, but
// only on the user's decision, never its own (the skill enforces that, and the
// user makes the call through Claude Code's AskUserQuestion prompt). An operator
// who wants approvals to be an unbreakable human-only act sets this to "1".
const strictApprovalEnv = "VESSEL_STRICT_APPROVAL"

// guardTrustBoundary gates a cage-widening command. By default it allows it: the
// human stays the decider by policy (the agent must ask before running one), not
// by a hard block. With VESSEL_STRICT_APPROVAL=1 it refuses when stdin is not a
// terminal, making the command a deliberate human-only act.
func guardTrustBoundary(cmd *cobra.Command, action string) error {
	if os.Getenv(strictApprovalEnv) != "1" {
		return nil
	}
	if stdinIsTerminal(cmd.InOrStdin()) {
		return nil
	}
	return fmt.Errorf("%s is restricted to a terminal by %s=1; run it in your own shell, or clear %s", action, strictApprovalEnv, strictApprovalEnv)
}
