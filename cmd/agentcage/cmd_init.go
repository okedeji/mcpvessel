package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/runtime"
)

func newInitCmd() *cobra.Command {
	var verbose bool
	var recreate bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Prepare the agentcage runtime (one-time setup)",
		Long: `Prepare the agentcage runtime on this host.

On macOS, agents run inside a small Linux VM provisioned by the bundled Lima
driver. The first run that needs the runtime creates the VM, downloads a Linux
image, and starts a rootless container daemon: 2-5 minutes depending on your
connection. After that every run is a few seconds; the VM stays up and the
daemon keeps cached images warm.

init runs that setup up front instead of inline. Skip it and the same setup
happens the first time you 'agentcage run', with the same progress UI.

On Linux this is a no-op: the host's containerd and buildkitd are used directly,
no VM.

--verbose streams the raw Lima output instead of the phase UI, for when setup is
going wrong. --recreate rebuilds the VM after a machine settings change (for
example raising machine.memory_gib): it stops the daemon, deletes the VM, and
provisions a fresh one, losing every cached image. On Linux --recreate just
restarts the daemon.`,
		Example: `  agentcage init
  agentcage init --verbose
  agentcage init --recreate`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
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
			if _, err := daemon.Ensure(ctx); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Runtime ready.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "stream the underlying provisioner output instead of the phase UI")
	cmd.Flags().BoolVar(&recreate, "recreate", false, "stop the daemon and rebuild the VM, applying a changed machine.memory_gib (macOS); deletes cached images")
	return cmd
}
