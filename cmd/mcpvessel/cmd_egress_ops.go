package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/egress"
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
	cmd.AddCommand(newEgressAllowCmd(), newEgressDenyCmd(), newEgressLsCmd(), newEgressPreviewCmd())
	return cmd
}

func newEgressAllowCmd() *cobra.Command {
	var once, all bool
	var agent string
	cmd := &cobra.Command{
		Use:   "allow TARGET HOST",
		Short: "Approve a held host (TARGET is a @tag or a run id)",
		Long: "Approve a held host. By default the host is granted to whichever " +
			"agents asked for it, so approving a host never opens it for an agent " +
			"that did not request it. Pass --agent NAME to pin the grant to one " +
			"named agent, or --all to grant it to every agent in the run.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if agent != "" && all {
				return fmt.Errorf("--agent and --all are mutually exclusive")
			}
			return decideEgress(cmd, args[0], args[1], agent, true, once, all)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "approve for the live run only; do not remember it in config")
	cmd.Flags().BoolVar(&all, "all", false, "grant the host to every agent in the run, not just the one that asked")
	cmd.Flags().StringVar(&agent, "agent", "", "grant the host to just this one named agent")
	return cmd
}

func newEgressDenyCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "deny TARGET HOST",
		Short: "Reject a host and stop it being held again this run",
		Long: "Reject a host a caged server is reaching. The host is dropped for the " +
			"rest of the run: every later attempt on it is refused fast instead of " +
			"held, so a rejected host never prompts you again. Denying also forgets " +
			"any remembered approval for the host. The refusals stay in `mcpvessel " +
			"audit`, but do not re-alert through the agent's watch.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decideEgress(cmd, args[0], args[1], agent, false, false, false)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "reject the host for just this one named agent")
	return cmd
}

func decideEgress(cmd *cobra.Command, target, host, agent string, allow, once, all bool) error {
	// Approving a host widens the cage, so it stays a human decision; denying
	// only tightens and needs no gate.
	if allow {
		if err := guardTrustBoundary(cmd, "egress allow"); err != nil {
			return err
		}
	}
	socket, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	client := daemon.Dial(socket)
	runIDs, configKey := resolveEgressTarget(cmd.Context(), client, target)

	released := 0
	for _, id := range runIDs {
		if err := client.AllowEgress(cmd.Context(), id, host, agent, allow, all); err != nil {
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
	scope := "for the requesting agent"
	switch {
	case all:
		scope = "for every agent in the run"
	case agent != "":
		scope = "for agent " + agent
	}
	msg := fmt.Sprintf("%s %s", verb, host)
	if released > 0 {
		if allow {
			msg += fmt.Sprintf(" %s in %d live run(s)", scope, released)
		} else {
			msg += fmt.Sprintf(" for %d live run(s)", released)
		}
	} else if allow {
		// Releasing nothing is normal when pre-approving a host before a run
		// exists, and alarming when the operator was answering a live hold. Say
		// which happened instead of burying it in a parenthetical.
		msg += "\nnote: no live run was holding this host, so nothing was released." +
			" If a server is held right now, name it as 'mcpvessel ps' shows it, or use its run id."
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
	if len(runIDs) > 0 {
		return runIDs, persistableRef(target)
	}
	// An untagged tag names the server without pinning a version. Exact-matching
	// alone let `egress allow @me/weather <host>` report success while releasing
	// nothing, because the live run's ref is @me/weather:0.1; the operator was
	// told the host was allowed and the cage stayed held.
	for _, r := range runs {
		if egressLive(r.Status) && sameRepository(r.Ref, target) {
			runIDs = append(runIDs, r.ID)
		}
	}
	return runIDs, persistableRef(target)
}

// sameRepository reports whether ref names the same server as an untagged
// target. Only an untagged target widens like this: given a version, the
// operator meant that version.
func sameRepository(ref, target string) bool {
	want, err := reference.Parse(target)
	if err != nil || want.Tag != "" || want.Digest != "" {
		return false
	}
	got, err := reference.Parse(ref)
	if err != nil {
		return false
	}
	return got.Registry == want.Registry && got.Repository == want.Repository
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

// heldHost is one held egress decision, as emitted by `egress ls --json` so an
// agent can read what is pending without scraping the human lines.
type heldHost struct {
	Run         string `json:"run"`           // the run id holding the host
	Ref         string `json:"ref,omitempty"` // the agent ref, when known
	Host        string `json:"host"`          // the host awaiting a decision
	Previewable bool   `json:"previewable"`   // a captured request is available via `egress preview`
}

func newEgressLsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
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
			refByID := map[string]string{}
			if runs, err := client.ListRuns(cmd.Context()); err == nil {
				for _, r := range runs {
					refByID[r.ID] = r.Ref
				}
			}
			previewable := map[string]bool{}
			if previews, err := client.PreviewableEgress(cmd.Context()); err == nil {
				for id, hosts := range previews {
					for _, h := range hosts {
						previewable[id+"\x00"+h] = true
					}
				}
			}
			if jsonOut {
				held := []heldHost{}
				for id, hosts := range pending {
					for _, h := range hosts {
						held = append(held, heldHost{
							Run:         id,
							Ref:         refByID[id],
							Host:        h,
							Previewable: previewable[id+"\x00"+h],
						})
					}
				}
				return writeJSON(cmd.OutOrStdout(), held)
			}
			if len(pending) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No hosts are being held.")
				return nil
			}
			for id, hosts := range pending {
				held := id
				if ref := refByID[id]; ref != "" {
					held = ref
				}
				for _, h := range hosts {
					line := fmt.Sprintf("%s\theld by %s\tapprove: mcpvessel egress allow %s %s", h, held, id, h)
					if previewable[id+"\x00"+h] {
						line += fmt.Sprintf("\tpreview: mcpvessel egress preview %s %s", id, h)
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), line)
				}
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nApproving grants the host to that agent only; add --all to grant every agent in the run.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

// newEgressPreviewCmd shows the full request a cage wants to send a held host,
// captured under --egress-inspect, so the operator can read exactly what is
// about to leave before approving. Unlike the live log line, this includes the
// body, on the operator's own terminal, on demand.
func newEgressPreviewCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "preview TARGET HOST",
		Short: "Show the request a cage wants to send a held host, before approving",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			client := daemon.Dial(socket)
			runIDs, _ := resolveEgressTarget(cmd.Context(), client, args[0])
			if len(runIDs) == 0 {
				runIDs = []string{args[0]} // try TARGET as a run id directly
			}
			for _, id := range runIDs {
				prev, err := client.FetchEgressPreview(cmd.Context(), id, args[1])
				if err == nil && prev != nil && prev.Method != "" {
					if jsonOut {
						return writeJSON(cmd.OutOrStdout(), prev)
					}
					_, _ = fmt.Fprint(cmd.OutOrStdout(), egress.FormatPreview(args[1], prev))
					return nil
				}
			}
			return fmt.Errorf("no pending preview for %s on %s; 'mcpvessel egress ls' lists what is held", args[1], args[0])
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}
