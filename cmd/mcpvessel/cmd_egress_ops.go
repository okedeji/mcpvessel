package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/reference"
)

// newEgressCmd is the operator's egress approval command. A run is deny-default:
// when a caged server reaches a host it is not yet allowed, the connection is
// held and surfaced in run/serve output and 'mcpvessel events'. This command
// approves or rejects the held host and, for an approval, remembers it in config
// so the next run of that tag does not ask again.
func newEgressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Approve or reject a host a running agent is held on",
		Long: `Approve or reject an outbound host a caged server is trying to reach.

A run is deny-default. The first time a server reaches a new host, the connection
is held: run/serve output and 'mcpvessel events' show it, and the call waits.
Approve it here, by the tag you ran (@org/name:version) or the run id from
'mcpvessel ps', and the held call proceeds. An approval is remembered in your
config for that tag, so future runs do not ask; --once approves the live run
only. 'egress deny' rejects a host and forgets a remembered approval.`,
		Example: `  mcpvessel egress allow @me/github:0.1 api.github.com
  mcpvessel egress allow researcher-7a1c4f2e9d3b api.github.com --once
  mcpvessel egress deny @me/github:0.1 evil.example.com
  mcpvessel egress ls`,
	}
	cmd.AddCommand(newEgressAllowCmd(), newEgressDenyCmd(), newEgressLsCmd())
	return cmd
}

func newEgressAllowCmd() *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "allow TARGET HOST",
		Short: "Approve a held host (TARGET is a @tag or a run id)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decideEgress(cmd, args[0], args[1], true, once)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "approve for the live run only; do not remember it in config")
	return cmd
}

func newEgressDenyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deny TARGET HOST",
		Short: "Reject a held host and forget any remembered approval",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decideEgress(cmd, args[0], args[1], false, false)
		},
	}
}

func decideEgress(cmd *cobra.Command, target, host string, allow, once bool) error {
	socket, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	client := daemon.Dial(socket)
	runIDs, configKey := resolveEgressTarget(cmd.Context(), client, target)

	released := 0
	for _, id := range runIDs {
		if err := client.AllowEgress(cmd.Context(), id, host, allow); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", id, err)
			continue
		}
		released++
	}

	persisted := false
	if configKey != "" && (allow && !once || !allow) {
		c, err := config.Load()
		if err != nil {
			return err
		}
		if allow {
			c.AddEgress(configKey, host)
		} else {
			c.RemoveEgressHost(configKey, host)
		}
		if err := c.Save(); err != nil {
			return err
		}
		persisted = true
	}

	verb := "Allowed"
	if !allow {
		verb = "Denied"
	}
	msg := fmt.Sprintf("%s %s", verb, host)
	if released > 0 {
		msg += fmt.Sprintf(" for %d live run(s)", released)
	} else {
		msg += " (no live run held this host)"
	}
	switch {
	case persisted && allow:
		msg += fmt.Sprintf("; remembered for %s", configKey)
	case persisted && !allow:
		msg += fmt.Sprintf("; forgotten for %s", configKey)
	case allow && once:
		msg += "; not remembered (--once)"
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), msg)
	return nil
}

// resolveEgressTarget maps a @tag or run id to the live run ids it names and the
// config key to persist under. A run id yields that run plus its ref as the key;
// a tag yields every live run of that ref, keyed by the tag itself.
func resolveEgressTarget(ctx context.Context, client *daemon.Client, target string) (runIDs []string, configKey string) {
	runs, err := client.ListRuns(ctx)
	if err != nil {
		runs = nil // no live runs to release; a tag can still be persisted
	}
	for _, r := range runs {
		if r.ID == target && egressLive(r.Status) {
			return []string{r.ID}, persistableRef(r.Ref)
		}
	}
	for _, r := range runs {
		if r.Ref == target && egressLive(r.Status) {
			runIDs = append(runIDs, r.ID)
		}
	}
	return runIDs, persistableRef(target)
}

// egressLive reports whether a run is still up enough to hold egress, so a
// finished run's torn-down proxy is not exec'd (which would error spuriously).
func egressLive(status string) bool {
	return status == "running" || status == "serving"
}

// persistableRef returns s as a config key only if it is a real @org/name[:tag]
// or host/org/name reference, so a content-hash display or a bare run id is not
// stored as a bogus key.
func persistableRef(s string) string {
	r, err := reference.Parse(s)
	if err != nil || !strings.Contains(r.Repository, "/") {
		return ""
	}
	return s
}

func newEgressLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List hosts running agents are held on, awaiting approval",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			client := daemon.Dial(socket)
			pending, err := client.PendingEgress(cmd.Context())
			if err != nil {
				return err
			}
			if len(pending) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No hosts are being held.")
				return nil
			}
			refByID := map[string]string{}
			if runs, err := client.ListRuns(cmd.Context()); err == nil {
				for _, r := range runs {
					refByID[r.ID] = r.Ref
				}
			}
			for id, hosts := range pending {
				held := id
				if ref := refByID[id]; ref != "" {
					held = ref
				}
				for _, h := range hosts {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\theld by %s\tapprove: mcpvessel egress allow %s %s\n", h, held, id, h)
				}
			}
			return nil
		},
	}
}
