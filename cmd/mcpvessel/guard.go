package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/config"
)

// strictApprovalEnv, set to "1", turns on the stricter posture where the
// cage-widening commands (approving an egress host, binding a secret,
// persisting an allow-list) refuse to run without an interactive terminal, so
// no agent can perform them at all. `mcpvessel config approvals set --strict`
// is the durable form of the same switch.
//
// It is off by default: an agent driving mcpvessel may run these commands, but
// only on the user's decision, never its own (the skill enforces that, and the
// user makes the call through Claude Code's AskUserQuestion prompt). An operator
// who wants approvals to be a human-only act turns it on.
const strictApprovalEnv = "VESSEL_STRICT_APPROVAL"

// strictApproval reports whether the strict posture is on.
//
// The config setting wins outright, and the environment can only add to it. That
// asymmetry is the whole point: the agent this restricts is the one that spawns
// the mcpvessel process, so it chooses that process's environment. If clearing
// VESSEL_STRICT_APPROVAL in the command line were enough to switch the
// restriction off, the restriction would not be one. The config file is under
// the operator's home at 0600, outside what running a command can change.
func strictApproval() bool {
	if cfg, err := config.Load(); err == nil && cfg.Approvals.Strict {
		return true
	}
	return os.Getenv(strictApprovalEnv) == "1"
}

// guardTrustBoundary gates a cage-widening command. By default it allows it: the
// human stays the decider by policy (the agent must ask before running one), not
// by a hard block. Under the strict posture it refuses when stdin is not a
// terminal, making the command a deliberate human-only act.
func guardTrustBoundary(cmd *cobra.Command, action string) error {
	if !strictApproval() {
		return nil
	}
	if stdinIsTerminal(cmd.InOrStdin()) {
		return nil
	}
	return fmt.Errorf("%s is restricted to a terminal by the strict approval policy; run it in your own shell, or clear it with 'mcpvessel config approvals set --strict=false' (and unset %s)", action, strictApprovalEnv)
}
