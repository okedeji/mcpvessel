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
	root.AddCommand(newBuildCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newCallCmd())
	root.AddCommand(newPushCmd())
	root.AddCommand(newPullCmd())
	root.AddCommand(newLoginCmd())
	root.AddCommand(newInspectCmd())
	root.AddCommand(newTreeCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newSecretsCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newServeCmd())
	root.AddCommand(newPsCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newSpendCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newBudgetCmd())
	root.AddCommand(newMCPGatewayCmd())
	root.AddCommand(newMCPControlCmd())
	root.AddCommand(newLLMGatewayCmd())
	root.AddCommand(newLLMControlCmd())
	root.AddCommand(newEgressCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
