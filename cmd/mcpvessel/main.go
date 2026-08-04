package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/identity"
)

func main() {
	bundle.SetBuiltWith(identity.Name + " " + identity.Version)

	// The Short and the group order are what someone arriving from the README
	// reads first, so they lead with the thing they came for: running a server
	// nobody has vetted without handing it the machine. The rest of the surface
	// (composing agents, reasoning, budgets, evals) is real and stays, but it is
	// not the introduction.
	root := &cobra.Command{
		Use:   identity.Name,
		Short: "Run untrusted MCP servers in isolated cages",
		Long: `Run untrusted MCP servers in isolated cages.

An MCP server normally runs as a subprocess with your full permissions: your
files, your keys, your network. mcpvessel runs each one alone in a container on
an isolated network, with its outbound traffic filtered by a gateway that opens
only the hosts you allow, and your secrets held outside the cage.

Start with 'mcpvessel init', then let Claude drive it, or cage a server yourself
with 'import' and put it behind one endpoint with 'serve'.`,
		Version:       identity.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// A bare --version also reports the docs server this machine is serving; see
	// version.go for why that half cannot be compiled in. Appended only for a
	// version request, so no other command pays for the daemon dial.
	if versionRequested(os.Args) {
		if line := docsServerVersion(context.Background()); line != "" {
			root.Version += "\n" + line
		}
	}

	// Groups shape --help by purpose; every command stays a top-level verb,
	// Docker-style. The order is the order a new user meets them: set up, cage
	// something, watch it, and only then the build and distribution half.
	root.AddGroup(
		&cobra.Group{ID: "setup", Title: "Setup:"},
		&cobra.Group{ID: "cage", Title: "Cage and serve an MCP server:"},
		&cobra.Group{ID: "observe", Title: "Watch what a caged server does:"},
		&cobra.Group{ID: "run", Title: "Run:"},
		&cobra.Group{ID: "ship", Title: "Build & distribute:"},
		&cobra.Group{ID: "configure", Title: "Configure:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("setup", newInitCmd(), newDaemonCmd())
	root.AddCommand(newVersionCmd())
	add("cage", newSearchCmd(), newImportCmd(), newServeCmd(), newEgressCmd(), newPsCmd(), newStopCmd(), newInspectCmd())
	add("observe", newAuditCmd(), newLogsCmd(), newEventsCmd(), newTraceCmd(), newStatsCmd(), newReplayCmd())
	add("run", newRunCmd(), newCallCmd(), newEvalCmd(), newSpendCmd(), newBudgetCmd())
	add("ship", newBuildCmd(), newPushCmd(), newPullCmd(), newRegisterCmd(), newLoginCmd(), newTreeCmd(), newStoreCmd())
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
