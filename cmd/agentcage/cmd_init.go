package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/runtime"
)

func newInitCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Prepare the agentcage runtime (one-time setup)",
		Long: `Prepare the agentcage runtime on this host.

On macOS, agentcage runs agents inside a small Linux VM provisioned
by the bundled Lima driver. The first time you do anything that
needs the runtime, that VM gets created, a Linux image is
downloaded, and a rootless container daemon is started. The whole
process takes 2-5 minutes depending on your connection. After it
completes, every later run is just a few seconds; the VM stays
around and the daemon keeps the cached images warm.

'agentcage init' runs that setup explicitly so you can do it on your
own time, not as a surprise mid-demo. If you skip 'init' and run
'agentcage run' directly, the same setup happens inline with the
same progress UI; 'init' is the polite version.

On Linux, this is a no-op: the host's containerd and buildkitd are
used directly and no VM is involved.

Pass --verbose to see the underlying Lima output instead of the
phase-by-phase UI. Useful when something is going wrong and the
clean view does not have enough detail.

Examples:

  agentcage init
  agentcage init --verbose`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			provisioner, err := runtime.DefaultProvisioner()
			if err != nil {
				return err
			}
			defer func() { _ = provisioner.Close() }()

			// Bring the runtime (the Lima VM on macOS) up first, behind the
			// phase UI, so the daemon we start next finds it ready instead of
			// provisioning silently into its log.
			if !runtime.SetupAlreadyReady(ctx, provisioner) {
				stderr := cmd.ErrOrStderr()
				ui := runtime.NewSetupUI(stderr)
				err := runtime.EnsureBootstrap(ctx, provisioner, ui, verbose, stderr)
				ui.Done()
				if err != nil {
					return err
				}
			}

			// Start the daemon so the first run, ps, or stop finds it already
			// up. Doing it here is the point of init: get the surprise latency
			// out of the way on the operator's own time.
			if _, err := daemon.Ensure(ctx); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Runtime ready.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "stream the underlying provisioner output instead of the phase UI")
	return cmd
}
