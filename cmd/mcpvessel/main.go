package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/identity"
)

func main() {
	bundle.SetBuiltWith(identity.Name + " " + identity.Version)

	root := &cobra.Command{
		Use:           identity.Name,
		Short:         "Build, ship, and run agents",
		Version:       identity.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Groups shape --help by purpose; every command stays a top-level verb, Docker-style.
	root.AddGroup(
		&cobra.Group{ID: "setup", Title: "Setup:"},
		&cobra.Group{ID: "ship", Title: "Build & distribute:"},
		&cobra.Group{ID: "run", Title: "Run:"},
		&cobra.Group{ID: "observe", Title: "Observe:"},
		&cobra.Group{ID: "configure", Title: "Configure:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("setup", newInitCmd(), newDaemonCmd())
	add("ship", newBuildCmd(), newImportCmd(), newPushCmd(), newPullCmd(), newRegisterCmd(), newSearchCmd(), newLoginCmd(), newInspectCmd(), newTreeCmd(), newStoreCmd())
	add("run", newRunCmd(), newCallCmd(), newEvalCmd(), newServeCmd(), newEgressCmd(), newStopCmd(), newBudgetCmd())
	add("observe", newPsCmd(), newAuditCmd(), newLogsCmd(), newSpendCmd(), newEventsCmd(), newTraceCmd(), newStatsCmd(), newReplayCmd())
	add("configure", newConfigCmd(), newSecretsCmd(), newKeysCmd(), newTrustCmd(), newSkillCmd())

	// Hidden internal commands the runtime execs inside gateway and cage containers.
	root.AddCommand(newMCPGatewayCmd(), newMCPControlCmd(), newLLMGatewayCmd(), newLLMControlCmd(), newEgressProxyCmd(), newEgressControlCmd(), newMCPBridgeCmd())

	// Hidden hook entrypoints Claude Code execs from ~/.claude/settings.json.
	root.AddCommand(newHookCmd())

	rejectUnknownSubcommands(root)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// rejectUnknownSubcommands walks the tree and gives every runless group
// command (config, secrets, store, ...) a RunE that shows help when called
// bare but errors, exit 1, on an unknown subcommand. Cobra's default for a
// runless parent prints help and exits 0 either way, so a typo like
// 'config ls' would read as success to a script.
func rejectUnknownSubcommands(cmd *cobra.Command) {
	if cmd.HasSubCommands() && cmd.Run == nil && cmd.RunE == nil {
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return fmt.Errorf("unknown command %q for %q; run '%s --help' for usage", args[0], cmd.CommandPath(), cmd.CommandPath())
		}
	}
	for _, sub := range cmd.Commands() {
		rejectUnknownSubcommands(sub)
	}
}
