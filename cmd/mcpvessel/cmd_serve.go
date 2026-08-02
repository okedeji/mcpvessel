package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/locate"
	"github.com/okedeji/mcpvessel/internal/progress"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/runtime"
	"github.com/okedeji/mcpvessel/internal/store"
)

func newServeCmd() *cobra.Command {
	var listen, budget string
	var expose, noExpose, egressFlags, secretFlags, envFlags []string
	var secretFile, envFile string
	var save, inspectEgress bool
	cmd := &cobra.Command{
		Use:   "serve BUNDLE...",
		Short: "Serve agents to external MCP clients over HTTP",
		Long: `Serve agents to external MCP clients over HTTP.

Each BUNDLE is a reference (resolved store-first, then pulled), a content hash
from an untagged build, a path to a .agent file, or a source directory with a
Vesselfile; a directory already built or imported serves its stored bundle
without a rebuild.

serve opens one front door for everything named. The merged endpoint at /mcp
advertises every public tool at once as <agent>_<tool>, so an MCP client
(Cursor, Claude) configures a single URL no matter how many bundles sit behind
it, and adding a bundle never renames an existing tool. Each exposed agent
also gets its own endpoint under /agents/, where tools keep their bare names.

A named agent is exposed; so is any USES PUBLIC sub-agent in its tree.
Transitive private sub-agents stay unreachable. --expose and --no-expose
override per agent, matched by repository, and --no-expose wins.

serve talks to the daemon, so it needs one running. It returns once the front
door is open; the daemon keeps serving until you 'mcpvessel stop' the runs or it
shuts down.`,
		Example: `  mcpvessel serve --listen :7000 @me/researcher:0.1
  mcpvessel serve --listen 127.0.0.1:7000 ./server-github ./mcp-server-time
  mcpvessel serve --listen 127.0.0.1:7000 --no-expose @me/creddb @me/researcher:0.1`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			envPool, secretPool, err := buildInputPools(envFlags, envFile, secretFlags, secretFile)
			if err != nil {
				return err
			}
			scoped := egress.ParseScoped(egressFlags)
			// --save bakes the egress into editable targets and rebuilds; the
			// runtime override then has nothing left to add. Without --save the
			// hosts are allowed for this serve only, and never touch the bundle.
			runtimeEgress := scoped
			if save {
				if err := saveEgress(cmd.Context(), cmd.ErrOrStderr(), args, scoped, envPool, secretPool.Flatten()); err != nil {
					return err
				}
				runtimeEgress = nil
			}
			targets := make([]daemon.ServeTarget, len(args))
			for i, arg := range args {
				if targets[i], err = resolveServeTarget(cmd.Context(), cmd.ErrOrStderr(), arg); err != nil {
					return err
				}
				if err := applyConfigSecrets(secretPool, targets[i].Ref, cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			policies, err := prebuildServeImages(cmd.Context(), cmd.ErrOrStderr(), targets, expose, noExpose)
			if err != nil {
				return err
			}
			var budgetMicros int64
			if budget != "" {
				m, err := parseUSDMicros(budget)
				if err != nil {
					return fmt.Errorf("--budget %q is not a USD amount", budget)
				}
				if m == 0 {
					return fmt.Errorf("--budget must be a positive amount; omit it to leave each instance unbounded")
				}
				budgetMicros = m
			}
			res, err := daemon.Dial(socket).Serve(cmd.Context(), targets, listen, expose, noExpose, runtimeEgress, envPool, secretPool, budgetMicros, inspectEgress)
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("cannot reach the mcpvessel daemon, run 'mcpvessel init' to start it: %w", err)
				}
				return err
			}

			for _, warning := range res.Warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", warning)
			}
			printServeReport(cmd.OutOrStdout(), res, policies, scoped, secretPool, inspectEgress)
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address to bind the MCP front door to, e.g. :7000 (required)")
	cmd.Flags().StringVar(&budget, "budget", "", "cap each client instance's LLM spend in USD, e.g. 5.00 (per instance, not shared)")
	cmd.Flags().StringArrayVar(&expose, "expose", nil, "also expose this agent, matched by repository (repeatable)")
	cmd.Flags().StringArrayVar(&noExpose, "no-expose", nil, "hide this agent even if USES PUBLIC, matched by repository (repeatable)")
	cmd.Flags().StringArrayVar(&egressFlags, "egress", nil, "allow a served agent hosts for this run: host,host, or agent:host,host to scope one of several (repeatable)")
	cmd.Flags().BoolVar(&save, "save", false, "with --egress, write the hosts into the agent's Vesselfile and rebuild instead of allowing them for this run only (source directories only)")
	cmd.Flags().BoolVar(&inspectEgress, "egress-inspect", false, "decrypt each served cage's outbound HTTPS to an approved host to record what it sent (opt-in; the proxy otherwise never sees payloads)")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME a served agent needs, or agent:NAME to grant one agent of several; the value resolves from your environment or the mcpvessel secret store (repeatable)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read secret values ([agent:]NAME=VALUE per line) from a perms-restricted file")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value a served agent needs: KEY=VALUE, or KEY to pass it through from your environment (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	_ = cmd.MarkFlagRequired("listen")
	cmd.AddCommand(newServeAddCmd(), newServeRmCmd())
	return cmd
}

// newServeAddCmd attaches more bundles to a running front door, merging their
// tools into the one endpoint a client already points at, instead of opening a
// second one. The client must reconnect to see the new tools.
func newServeAddCmd() *cobra.Command {
	var listen, budget string
	var expose, noExpose, egressFlags, secretFlags, envFlags []string
	var secretFile, envFile string
	var inspectEgress bool
	cmd := &cobra.Command{
		Use:   "add BUNDLE...",
		Short: "Add bundles to a running front door, merged into its one endpoint",
		Long: `Add bundles to a front door already opened by 'mcpvessel serve', merging their
tools into the same endpoint the client points at, so the client keeps one MCP
server entry no matter how many bundles sit behind it.

You are adding a new MCP server; mcpvessel just surfaces it merged into the one.
So the merged tool list changes, and your MCP client must reconnect (restart the
session, or re-add the same URL) to pick up the new tools.

--listen selects the front door; it is inferred when only one is running.`,
		Example: `  mcpvessel serve add @me/github:0.1
  mcpvessel serve add --listen 127.0.0.1:7000 ./mcp-server-time`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			client := daemon.Dial(socket)
			listen, err = inferServeListen(cmd.Context(), client, listen)
			if err != nil {
				return err
			}
			envPool, secretPool, err := buildInputPools(envFlags, envFile, secretFlags, secretFile)
			if err != nil {
				return err
			}
			scoped := egress.ParseScoped(egressFlags)
			targets := make([]daemon.ServeTarget, len(args))
			for i, arg := range args {
				if targets[i], err = resolveServeTarget(cmd.Context(), cmd.ErrOrStderr(), arg); err != nil {
					return err
				}
				if err := applyConfigSecrets(secretPool, targets[i].Ref, cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			// Before the build, not after: a wrong --listen used to spend a full
			// image build before failing, and the failure arrived as a wall of
			// build output with the actual problem nowhere in it.
			if err := requireOpenDoor(cmd.Context(), client, listen); err != nil {
				return err
			}
			policies, err := prebuildServeImages(cmd.Context(), cmd.ErrOrStderr(), targets, expose, noExpose)
			if err != nil {
				return err
			}
			var budgetMicros int64
			if budget != "" {
				m, err := parseUSDMicros(budget)
				if err != nil {
					return fmt.Errorf("--budget %q is not a USD amount", budget)
				}
				budgetMicros = m
			}
			res, err := client.ServeAdd(cmd.Context(), targets, listen, expose, noExpose, scoped, envPool, secretPool, budgetMicros, inspectEgress)
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("cannot reach the mcpvessel daemon, run 'mcpvessel init' to start it: %w", err)
				}
				return err
			}
			for _, warning := range res.Warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", warning)
			}
			printServeReport(cmd.OutOrStdout(), res, policies, scoped, secretPool, inspectEgress)
			if res.RestartClient {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nReconnect your MCP client (restart the session, or re-add %s) to load the new tools.\n", "http://"+listen+serveFlatPath())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "front door address (inferred when only one is running)")
	cmd.Flags().StringVar(&budget, "budget", "", "cap each added instance's LLM spend in USD, e.g. 5.00")
	cmd.Flags().StringArrayVar(&expose, "expose", nil, "also expose this agent, matched by repository (repeatable)")
	cmd.Flags().StringArrayVar(&noExpose, "no-expose", nil, "hide this agent even if USES PUBLIC, matched by repository (repeatable)")
	cmd.Flags().StringArrayVar(&egressFlags, "egress", nil, "allow the added agent hosts for this run: host,host, or agent:host,host to scope one (repeatable)")
	cmd.Flags().BoolVar(&inspectEgress, "egress-inspect", false, "decrypt the added cage's outbound HTTPS to an approved host to record what it sent")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME the added agent needs, or agent:NAME to scope it (repeatable)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read secret values ([agent:]NAME=VALUE per line) from a perms-restricted file")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value the added agent needs: KEY=VALUE, or KEY to pass through (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	return cmd
}

// newServeRmCmd detaches a served bundle from a running front door, rebuilding
// the merged endpoint without it. REF is an agent address (as `ps` shows it) or
// the ref the bundle was served under.
func newServeRmCmd() *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "rm REF",
		Short: "Remove a served bundle from a running front door",
		Long: `Detach a served bundle from a front door and rebuild its merged endpoint without
that bundle's tools. When it was the last bundle, the front door closes and frees
its port. Your MCP client must reconnect to drop the removed tools.

REF is an agent address (as 'mcpvessel ps' lists it) or the ref it was served
under. --listen is inferred when only one front door is running.`,
		Example: `  mcpvessel serve rm @me/github:0.1
  mcpvessel serve rm --listen 127.0.0.1:7000 github`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			client := daemon.Dial(socket)
			listen, err = inferServeListen(cmd.Context(), client, listen)
			if err != nil {
				return err
			}
			res, err := client.ServeRemove(cmd.Context(), listen, args[0])
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("cannot reach the mcpvessel daemon, run 'mcpvessel init' to start it: %w", err)
				}
				return err
			}
			if res.Closed {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s; the front door on %s is now closed.\n", args[0], listen)
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from %s. Reconnect your MCP client to drop its tools.\n", args[0], listen)
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "front door address (inferred when only one is running)")
	return cmd
}

// inferServeListen returns explicit when set, otherwise the single running front
// door's address, erroring when there are none or several to choose from.
func inferServeListen(ctx context.Context, client *daemon.Client, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	runs, err := client.ListRuns(ctx)
	if err != nil {
		var unreachable *daemon.Unreachable
		if errors.As(err, &unreachable) {
			return "", fmt.Errorf("cannot reach the mcpvessel daemon, run 'mcpvessel init' to start it: %w", err)
		}
		return "", err
	}
	set := map[string]bool{}
	for _, r := range runs {
		if r.Status == "serving" && r.Endpoint != "" {
			if l := listenFromEndpoint(r.Endpoint); l != "" {
				set[l] = true
			}
		}
	}
	switch len(set) {
	case 0:
		return "", fmt.Errorf("no serve front door is running; start one with 'mcpvessel serve'")
	case 1:
		for l := range set {
			return l, nil
		}
	}
	ls := make([]string, 0, len(set))
	for l := range set {
		ls = append(ls, l)
	}
	sort.Strings(ls)
	return "", fmt.Errorf("several front doors are running (%s); pass --listen to pick one", strings.Join(ls, ", "))
}

// listenFromEndpoint pulls the bind address out of a serving run's endpoint URL
// (http://<listen>/agents/<addr>/mcp).
func listenFromEndpoint(ep string) string {
	ep = strings.TrimPrefix(ep, "http://")
	if i := strings.Index(ep, "/"); i >= 0 {
		return ep[:i]
	}
	return ep
}

// serveFlatPath is the merged endpoint path, for the reconnect hint.
func serveFlatPath() string { return "/mcp" }

// exposedPolicy is what the boot-time reports need from one exposed agent's
// manifest: its baked egress policy and its declared secrets.
type exposedPolicy struct {
	Egress   string
	Secrets  []string
	Optional []string
}

// prebuildServeImages builds, before the front door opens, every image the
// serve's instance boots will need: each exposed agent (the roots named plus
// their USES PUBLIC sub-agents, which serve boots as independent instances)
// gets its full tree built. Synchronous on purpose: a background build would
// only narrow the race with the client's first call, and a build failure
// belongs in this terminal, not inside an MCP error in Cursor. Everything is
// content-addressed, so already-built bundles cost an existence check.
//
// It also returns each exposed agent's baked EGRESS policy and declared
// secrets by address, for the boot-time reports: the daemon honors a
// bundle's baked hosts with no flag at all, and injects a granted secret
// into any agent declaring its name, so serve must show both before any
// traffic flows.
func prebuildServeImages(ctx context.Context, stderr io.Writer, targets []daemon.ServeTarget, expose, noExpose []string) (map[string]exposedPolicy, error) {
	prebuilt := map[string]bool{}
	baked := map[string]exposedPolicy{}
	for _, t := range targets {
		b, err := locate.Bundle(ctx, t.Ref)
		if err != nil {
			return nil, err
		}
		// Mirrors the daemon's root address derivation in handleServe, so the
		// addresses reported here are the ones it serves under.
		name := b.Name
		if ref, perr := reference.Parse(t.Ref); perr == nil && ref.Repository != "" {
			name = ref.Repository
		}
		if t.Name != "" {
			name = t.Name
		}
		exposed, err := runtime.ResolveExposure(ctx, b.Path, name, runtime.ExposureOverrides{
			Expose:   expose,
			NoExpose: noExpose,
		})
		if err != nil {
			return nil, err
		}
		for _, ea := range exposed {
			if m, err := bundle.ReadManifest(ea.Bundle); err == nil {
				baked[ea.Address] = exposedPolicy{
					Egress:   m.Vesselfile.Egress,
					Secrets:  m.Vesselfile.Secrets,
					Optional: m.Vesselfile.Optional,
				}
			}
			if prebuilt[ea.Bundle] {
				continue
			}
			prebuilt[ea.Bundle] = true
			if err := runtime.PrebuildImages(ctx, ea.Bundle, stderr); err != nil {
				return nil, fmt.Errorf("preparing images for %s: %w", ea.Address, err)
			}
		}
	}
	return baked, nil
}

// resolveServeTarget turns one serve argument into a daemon-resolvable
// target. A source directory with a Vesselfile resolves by content hash: the
// stored bundle is served as-is when present (an import or build already
// introspected it), else the directory is built into the store first. The
// directory's name becomes the agent's address; a hash prefix would make a
// poor one. Anything else passes through for the daemon's locate.
func resolveServeTarget(ctx context.Context, stderr io.Writer, arg string) (daemon.ServeTarget, error) {
	info, err := os.Stat(arg)
	if err != nil || !info.IsDir() {
		return daemon.ServeTarget{Ref: arg}, nil
	}
	if _, err := os.Stat(filepath.Join(arg, bundle.VesselfileName)); err != nil {
		// A directory without a Vesselfile still gets locate's clearer error.
		return daemon.ServeTarget{Ref: arg}, nil
	}

	st, err := store.New()
	if err != nil {
		return daemon.ServeTarget{}, err
	}
	name := filepath.Base(strings.TrimSuffix(arg, string(filepath.Separator)))
	hash, err := bundle.HashSource(arg, st.Dir())
	if err != nil {
		return daemon.ServeTarget{}, err
	}
	if _, statErr := os.Stat(st.PathFor(hash)); statErr == nil {
		return daemon.ServeTarget{Ref: hash, Name: name}, nil
	}

	hash, _, err = buildIntoStore(ctx, stderr, stderr, buildConfig{
		srcDir: arg,
		mode:   progress.ModeAuto,
	})
	if err != nil {
		return daemon.ServeTarget{}, err
	}
	return daemon.ServeTarget{Ref: hash, Name: name}, nil
}

// requireOpenDoor fails 'serve add' when no front door is listening on listen,
// naming the doors that are open. Without it the command builds first and fails
// afterwards, so the operator reads a build log for a mistake in a flag.
func requireOpenDoor(ctx context.Context, client *daemon.Client, listen string) error {
	runs, err := client.ListRuns(ctx)
	if err != nil {
		return nil // the daemon answers for itself on the real call
	}
	var open []string
	for _, r := range runs {
		if r.Status != "serving" || r.Endpoint == "" {
			continue
		}
		addr := strings.TrimSuffix(strings.TrimPrefix(r.Endpoint, "http://"), serveFlatPath())
		if addr == listen {
			return nil
		}
		if !slices.Contains(open, addr) {
			open = append(open, addr)
		}
	}
	if len(open) == 0 {
		return fmt.Errorf("no front door is open on %s, and none is open anywhere; open one with 'mcpvessel serve --listen %s <bundle>' instead of 'serve add'", listen, listen)
	}
	return fmt.Errorf("no front door is open on %s. Open doors: %s. Pass --listen with one of those to join it, or use 'mcpvessel serve' to open a new one", listen, strings.Join(open, ", "))
}

// unchangedByThisCommand marks an agent already on the door when this command
// ran. Its egress and secrets are whatever the command that served it set, and
// this process has no way to read them back, so the report says it does not
// know rather than reporting an absence it did not verify. Understating a cage's
// reach is the dangerous direction to be wrong in.
const unchangedByThisCommand = "already serving; egress and secrets unchanged by this command"

// printServeReport renders the serve boot report: the URL to paste into an
// MCP client first, then each agent's effective egress and secret grants,
// then the REST surface. One served agent collapses to a single endpoint;
// several keep the merged /mcp plus one endpoint each. Endpoints print as
// full URLs because the reader's next act is pasting one into a client.
func printServeReport(out io.Writer, res daemon.ServeResult, policies map[string]exposedPolicy, scoped map[string][]string, secretPool runtime.ScopedSecrets, inspect bool) {
	base := "http://" + res.Listen
	single := len(res.Agents) == 1
	if single {
		_, _ = fmt.Fprintf(out, "Serving %s on %s\n", res.Agents[0].Address, base)
	} else {
		_, _ = fmt.Fprintf(out, "Serving %d agents on %s\n", len(res.Agents), base)
	}

	_, _ = fmt.Fprintln(out)
	if single {
		_, _ = fmt.Fprintln(out, "MCP endpoint, point your client here:")
		_, _ = fmt.Fprintf(out, "  %s%s\n", base, res.Flat.Path)
		if len(res.Flat.Tools) > 0 {
			_, _ = fmt.Fprintf(out, "  %s\n", toolSummary(res.Flat.Tools))
		}
	} else {
		_, _ = fmt.Fprintln(out, "MCP endpoints, one URL for your MCP client:")
		if res.Flat.Path != "" {
			line := fmt.Sprintf("  %s%s  (all public tools)", base, res.Flat.Path)
			if len(res.Flat.Tools) > 0 {
				line += "  " + toolSummary(res.Flat.Tools)
			}
			_, _ = fmt.Fprintln(out, line)
		}
		for _, a := range res.Agents {
			line := fmt.Sprintf("  %s/agents/%s/mcp", base, a.Address)
			if len(a.Tools) > 0 {
				line += "  " + toolSummary(a.Tools)
			}
			_, _ = fmt.Fprintln(out, line)
		}
	}

	// The effective allowlist per agent, baked hosts included: a pulled
	// bundle's author-declared egress applies with no flag, so this is where
	// the operator sees it before any traffic flows.
	//
	// policies only covers the bundles this command named, while res.Agents is
	// every agent on the door. On 'serve add' the difference is the ones already
	// serving, whose policy this process never loaded: they are marked as
	// untouched rather than rendered from a zero value, which would print
	// "none preset" and "none declared" over a server that has neither.
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Egress:")
	for _, a := range res.Agents {
		pol, known := policies[a.Address]
		if !known {
			_, _ = fmt.Fprintf(out, "  %s: %s\n", a.Address, unchangedByThisCommand)
			continue
		}
		_, _ = fmt.Fprintf(out, "  %s: %s\n", a.Address,
			formatEgress(egress.AllowHosts(pol.Egress), egress.HostsFor(scoped, a.Address)))
	}
	// And which declared secrets each agent will actually receive: a
	// broadcast --secret reaches every agent declaring its name, agent:NAME
	// pins it to one.
	_, _ = fmt.Fprintln(out, "Secrets:")
	for _, a := range res.Agents {
		pol, known := policies[a.Address]
		if !known {
			_, _ = fmt.Fprintf(out, "  %s: %s\n", a.Address, unchangedByThisCommand)
			continue
		}
		_, _ = fmt.Fprintf(out, "  %s: %s\n", a.Address,
			formatSecretGrants(pol.Secrets, pol.Optional, secretPool.For(a.Address)))
	}

	if inspect {
		// Loud on purpose: inspection decrypts the cage's HTTPS, a deliberate
		// break from the default where the proxy never sees a payload.
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Egress inspection: ON")
		_, _ = fmt.Fprintln(out, "  Each cage's HTTPS to an approved host is decrypted to see what it sent.")
		_, _ = fmt.Fprintln(out, "  Requests show live in 'mcpvessel events' and 'mcpvessel logs' as metadata")
		_, _ = fmt.Fprintln(out, "  (method, host, sizes). For full bodies, 'mcpvessel replay record --egress-inspect'.")
	}

	// The prompt endpoint exists only for a MAIN-bearing agent, so it is
	// advertised only when one is being served, by name when unambiguous.
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "REST on the same port:")
	toolName := "<name>"
	if single {
		toolName = res.Agents[0].Address
	}
	_, _ = fmt.Fprintf(out, "  POST %s/agents/%s/tools/<tool>  JSON args in, JSON result out\n", base, toolName)
	var mains []string
	for _, a := range res.Agents {
		if a.Main != "" {
			mains = append(mains, a.Address)
		}
	}
	switch {
	case len(mains) == 1:
		_, _ = fmt.Fprintf(out, "  POST %s/agents/%s  prompt it with {\"prompt\": ...}; add {\"stream\": true} for SSE chunks\n", base, mains[0])
	case len(mains) > 1:
		_, _ = fmt.Fprintf(out, "  POST %s/agents/<name>  prompt an agent with {\"prompt\": ...}; add {\"stream\": true} for SSE chunks\n", base)
	}
}

// toolSummary caps a tool list so one well-stocked server cannot wrap the
// report off the terminal: the count, then up to eight names, then an
// ellipsis ('mcpvessel inspect' lists them all).
func toolSummary(names []string) string {
	noun := "tools"
	if len(names) == 1 {
		noun = "tool"
	}
	shown, suffix := names, ""
	if len(shown) > 8 {
		shown, suffix = shown[:8], ", ..."
	}
	return fmt.Sprintf("%d %s: %s%s", len(names), noun, strings.Join(shown, ", "), suffix)
}
