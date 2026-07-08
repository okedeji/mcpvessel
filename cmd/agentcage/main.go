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
	add("run", newRunCmd(), newCallCmd(), newEvalCmd(), newServeCmd(), newStopCmd(), newBudgetCmd())
	add("observe", newPsCmd(), newLogsCmd(), newSpendCmd(), newEventsCmd(), newTraceCmd(), newStatsCmd(), newReplayCmd())
	add("configure", newConfigCmd(), newSecretsCmd())

	// Hidden internal commands the runtime execs inside gateway and cage containers.
	root.AddCommand(newMCPGatewayCmd(), newMCPControlCmd(), newLLMGatewayCmd(), newLLMControlCmd(), newEgressCmd(), newMCPBridgeCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
