package main

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/clientskill"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/runtime"
)

func newInitCmd() *cobra.Command {
	var verbose bool
	var recreate bool
	var client string
	var skipSkill bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Prepare the mcpvessel runtime (one-time setup)",
		Long: `Prepare the mcpvessel runtime on this host.

On macOS, agents run inside a small Linux VM provisioned by the bundled Lima
driver. The first run that needs the runtime creates the VM, downloads a Linux
image, and starts a rootless container daemon: 2-5 minutes depending on your
connection. After that every run is a few seconds; the VM stays up and the
daemon keeps cached images warm.

init runs that setup up front instead of inline. Skip it and the same setup
happens the first time you 'mcpvessel run', with the same progress UI.

On Linux this is a no-op: the host's containerd and buildkitd are used directly,
no VM.

--verbose streams the raw Lima output instead of the phase UI, for when setup is
going wrong. --recreate rebuilds the VM after a machine settings change (for
example raising machine.memory_gib): it stops the daemon, deletes the VM, and
provisions a fresh one, losing every cached image. On Linux --recreate just
restarts the daemon.`,
		Example: `  mcpvessel init
  mcpvessel init --verbose
  mcpvessel init --recreate`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Fetch the pinned Lima bundle first if it is missing, before
			// DefaultProvisioner tries to locate limactl and fails. A no-op
			// off macOS and when limactl is already bundled or on PATH.
			if err := runtime.EnsureLimaAvailable(ctx, cmd.ErrOrStderr()); err != nil {
				return err
			}

			provisioner, err := runtime.DefaultProvisioner()
			if err != nil {
				return err
			}
			defer func() { _ = provisioner.Close() }()

			// Tear the VM down so the bootstrap below rebuilds it with the
			// current machine config. Stop the daemon first: recreating the
			// VM under it would orphan every container it holds. On Linux
			// there is no VM, so this is just a daemon restart.
			if recreate {
				stderr := cmd.ErrOrStderr()
				_, _ = fmt.Fprintln(stderr, "Recreating the runtime...")
				if _, err := daemon.Stop(ctx); err != nil {
					return fmt.Errorf("stopping the daemon before recreate: %w", err)
				}
				if err := provisioner.DestroyVM(ctx, io.Discard, stderr); err != nil {
					return fmt.Errorf("destroying the VM: %w", err)
				}
			}

			// Bring the runtime up behind the phase UI first, so the daemon
			// finds it ready instead of provisioning silently into its log.
			if !runtime.SetupAlreadyReady(ctx, provisioner) {
				stderr := cmd.ErrOrStderr()
				ui := runtime.NewSetupUI(stderr)
				err := runtime.EnsureBootstrap(ctx, provisioner, ui, verbose, stderr)
				ui.Done()
				if err != nil {
					return err
				}
			}

			// Taking the daemon start latency here is the point of init.
			dae, err := daemon.Ensure(ctx)
			if err != nil {
				return err
			}

			// Surface a missing in-VM companion now rather than at the first
			// run. Bundled in a release; a from-source tree needs a build.
			if _, err := runtime.FindLinuxBinary(); err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: the in-VM agent binary is not built yet; the first run needs it (run 'make build-linux' from source)")
			}

			// Set up the chosen MCP client so its agent can drive mcpvessel:
			// install the skill, then serve and register the caged docs server as
			// its reference. Every step degrades to a note; nothing here fails an
			// otherwise-ready runtime.
			if !skipSkill {
				clientID, err := installClientSkill(cmd, client)
				if err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: could not install the client skill: %v\n", err)
				} else if clientID != "" {
					installClientHooks(cmd, clientID)
					bootstrapDocs(cmd, dae, clientID)
				}
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Runtime ready.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "stream the underlying provisioner output instead of the phase UI")
	cmd.Flags().BoolVar(&recreate, "recreate", false, "stop the daemon and rebuild the VM, applying a changed machine.memory_gib (macOS); deletes cached images")
	cmd.Flags().StringVar(&client, "client", "", "set up this MCP client without prompting: install the skill and register the caged docs server (e.g. claude-code)")
	cmd.Flags().BoolVar(&skipSkill, "skip-skill", false, "skip the client setup entirely (skill and docs server)")
	return cmd
}

// installClientSkill installs the skill for the chosen MCP client and returns
// the resolved client id (so the caller can finish the client's bootstrap).
// With an explicit --client it installs directly; at a terminal it prompts; and
// non-interactively with no client it does nothing, so CI is untouched. An empty
// id means nothing was installed (skipped or declined).
func installClientSkill(cmd *cobra.Command, client string) (string, error) {
	if client == "" {
		if !isInteractive(cmd) {
			return "", nil
		}
		var err error
		client, err = promptClient(cmd)
		if err != nil || client == "" {
			return "", err
		}
	}
	res, err := clientskill.Install(client)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Installed the mcpvessel skill for %s at %s\n", res.ClientID, res.Path)
	return res.ClientID, nil
}

// installClientHooks wires the mcpvessel watch into the client as hooks, so it
// fires deterministically on session start and after every caged-tool call, not
// only when the skill happens to be loaded. Claude Code only today; best-effort,
// a failure never fails init.
func installClientHooks(cmd *cobra.Command, clientID string) {
	if clientID != "claude-code" {
		return
	}
	path, wrote, err := installClaudeHooks()
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: could not install the mcpvessel watch hooks: %v\n", err)
		return
	}
	if wrote {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Installed the mcpvessel watch hooks for Claude Code in %s\n", path)
	}
}

// bootstrapDocs serves the caged docs server (which also persists the opt-in for
// later startups) and registers its URL with the client, so the agent has a
// reference without the user setting anything up. Best-effort: if the bundle is
// unpublished or the client CLI is missing, it prints a note and returns.
func bootstrapDocs(cmd *cobra.Command, dae *daemon.Client, clientID string) {
	stderr := cmd.ErrOrStderr()
	res, err := dae.EnsureDocs(cmd.Context())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "note: could not set up the mcpvessel-docs server (it may not be published yet); the agent can still use the open-source repo. %v\n", err)
		return
	}
	registerDocsWithClient(cmd, clientID, res.URL)
}

// registerDocsWithClient adds the caged docs server to the MCP client. Only
// Claude Code has an install path today (the claude CLI); any other client gets
// the URL to add by hand. Idempotent for Claude Code: an existing entry is left
// alone.
func registerDocsWithClient(cmd *cobra.Command, clientID, url string) {
	stderr := cmd.ErrOrStderr()
	if clientID != "claude-code" {
		_, _ = fmt.Fprintf(stderr, "The mcpvessel docs server is caged and serving at %s. Register it with your client to let the agent query it.\n", url)
		return
	}

	claude, err := exec.LookPath("claude")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "The mcpvessel docs server is caged and serving at %s. The `claude` CLI is not on your PATH, so register it yourself: claude mcp add --scope user mcpvessel-docs --transport http %s\n", url, url)
		return
	}

	// Skip if it is already registered, so re-running init does not error on a
	// duplicate. `claude mcp get` exits non-zero when the entry is absent.
	if exec.Command(claude, "mcp", "get", docsMCPName).Run() == nil {
		_, _ = fmt.Fprintln(stderr, "The mcpvessel docs server is already registered with Claude Code.")
		return
	}

	add := exec.CommandContext(cmd.Context(), claude, "mcp", "add", "--scope", "user", docsMCPName, "--transport", "http", url)
	if out, err := add.CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(stderr, "note: could not register the docs server with Claude Code (%v); register it yourself: claude mcp add --scope user %s --transport http %s\n%s", err, docsMCPName, url, out)
		return
	}
	_, _ = fmt.Fprintf(stderr, "Registered the mcpvessel docs server with Claude Code (%s) at %s\n", docsMCPName, url)
}

// docsMCPName is the client-facing entry name for the caged docs server.
const docsMCPName = "mcpvessel-docs"

// promptClient shows the client menu and returns the selected id, or "" if the
// user declined or their client is not one mcpvessel packages a skill for.
func promptClient(cmd *cobra.Command) (string, error) {
	w := cmd.ErrOrStderr()
	list := clientskill.Clients()
	_, _ = fmt.Fprintln(w, "\nSet up your MCP client so its agent can drive mcpvessel (installs the skill and the caged docs server):")
	for i, c := range list {
		_, _ = fmt.Fprintf(w, "  [%d] %s\n", i+1, c.Name)
	}
	_, _ = fmt.Fprintln(w, "  (more clients coming)")
	_, _ = fmt.Fprint(w, "Which client, or Enter to skip? ")
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	choice := strings.TrimSpace(line)
	if choice == "" {
		return "", nil
	}
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(list) {
		_, _ = fmt.Fprintln(w, "Skipped. If your client is not listed, its skill is not packaged yet; the flow is documented at github.com/okedeji/mcpvessel.")
		return "", nil
	}
	return list[n-1].ID, nil
}
