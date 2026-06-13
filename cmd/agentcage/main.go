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
	root.AddCommand(newGatewayCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
