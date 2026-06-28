package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/identity"
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
	// Groups organize the commands in --help without nesting them: every command
	// stays a top-level verb, the way the Docker-mirrored shape wants, but the help
	// reads by purpose instead of one long list.
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
	add("ship", newBuildCmd(), newPushCmd(), newPullCmd(), newLoginCmd(), newInspectCmd(), newTreeCmd())
	add("run", newRunCmd(), newCallCmd(), newServeCmd(), newStopCmd(), newBudgetCmd())
	add("observe", newPsCmd(), newLogsCmd(), newSpendCmd(), newEventsCmd(), newTraceCmd(), newStatsCmd(), newReplayCmd())
	add("configure", newConfigCmd(), newSecretsCmd())

	// Internal commands the runtime execs inside the gateway and cage containers.
	// Hidden from help, so they need no group.
	root.AddCommand(newMCPGatewayCmd(), newMCPControlCmd(), newLLMGatewayCmd(), newLLMControlCmd(), newEgressCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
