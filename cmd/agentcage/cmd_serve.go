package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/daemon"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/progress"
	"github.com/okedeji/agentcage/internal/runtime"
	"github.com/okedeji/agentcage/internal/store"
)

func newServeCmd() *cobra.Command {
	var listen string
	var expose, noExpose []string
	cmd := &cobra.Command{
		Use:   "serve BUNDLE...",
		Short: "Serve agents to external MCP clients over HTTP",
		Long: `Serve agents to external MCP clients over HTTP.

Each BUNDLE is a reference (resolved store-first, then pulled), a content hash
from an untagged build, a path to a .agent file, or a source directory with an
Agentfile — a directory already built or imported serves its stored bundle
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
door is open; the daemon keeps serving until you 'agentcage stop' the runs or it
shuts down.`,
		Example: `  agentcage serve --listen :7000 @me/researcher:0.1
  agentcage serve --listen 127.0.0.1:7000 ./server-github ./mcp-server-time
  agentcage serve --listen 127.0.0.1:7000 --no-expose @me/creddb @me/researcher:0.1`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			targets := make([]daemon.ServeTarget, len(args))
			for i, arg := range args {
				if targets[i], err = resolveServeTarget(cmd.Context(), cmd.ErrOrStderr(), arg); err != nil {
					return err
				}
			}
			if err := prebuildServeImages(cmd.Context(), cmd.ErrOrStderr(), targets, expose, noExpose); err != nil {
				return err
			}
			res, err := daemon.Dial(socket).Serve(cmd.Context(), targets, listen, expose, noExpose)
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("cannot reach the agentcage daemon, run 'agentcage init' to start it: %w", err)
				}
				return err
			}

			for _, warning := range res.Warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", warning)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Serving %d agent(s) on %s\n", len(res.Agents), res.Listen)
			if res.Flat.Path != "" {
				_, _ = fmt.Fprintf(out, "  %s  (all public tools, one URL for your MCP client)", res.Flat.Path)
				if len(res.Flat.Tools) > 0 {
					_, _ = fmt.Fprintf(out, "  tools: %s", strings.Join(res.Flat.Tools, ", "))
				}
				_, _ = fmt.Fprintln(out)
			}
			for _, a := range res.Agents {
				_, _ = fmt.Fprintf(out, "  /agents/%s/mcp", a.Address)
				if len(a.Tools) > 0 {
					_, _ = fmt.Fprintf(out, "  tools: %s", strings.Join(a.Tools, ", "))
				}
				_, _ = fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address to bind the MCP front door to, e.g. :7000 (required)")
	cmd.Flags().StringArrayVar(&expose, "expose", nil, "also expose this agent, matched by repository (repeatable)")
	cmd.Flags().StringArrayVar(&noExpose, "no-expose", nil, "hide this agent even if USES PUBLIC, matched by repository (repeatable)")
	_ = cmd.MarkFlagRequired("listen")
	return cmd
}

// prebuildServeImages builds, before the front door opens, every image the
// serve's instance boots will need: each exposed agent (the roots named plus
// their USES PUBLIC sub-agents, which serve boots as independent instances)
// gets its full tree built. Synchronous on purpose — a background build would
// only narrow the race with the client's first call, and a build failure
// belongs in this terminal, not inside an MCP error in Cursor. Everything is
// content-addressed, so already-built bundles cost an existence check.
func prebuildServeImages(ctx context.Context, stderr io.Writer, targets []daemon.ServeTarget, expose, noExpose []string) error {
	prebuilt := map[string]bool{}
	for _, t := range targets {
		b, err := locate.Bundle(ctx, t.Ref)
		if err != nil {
			return err
		}
		name := t.Name
		if name == "" {
			name = b.Name
		}
		exposed, err := runtime.ResolveExposure(ctx, b.Path, name, runtime.ExposureOverrides{
			Expose:   expose,
			NoExpose: noExpose,
		})
		if err != nil {
			return err
		}
		for _, ea := range exposed {
			if prebuilt[ea.Bundle] {
				continue
			}
			prebuilt[ea.Bundle] = true
			if err := runtime.PrebuildImages(ctx, ea.Bundle, stderr); err != nil {
				return fmt.Errorf("preparing images for %s: %w", ea.Address, err)
			}
		}
	}
	return nil
}

// resolveServeTarget turns one serve argument into a daemon-resolvable
// target. A source directory with an Agentfile resolves by content hash: the
// stored bundle is served as-is when present (an import or build already
// introspected it), else the directory is built into the store first. The
// directory's name becomes the agent's address — a hash prefix would make a
// poor one. Anything else passes through for the daemon's locate.
func resolveServeTarget(ctx context.Context, stderr io.Writer, arg string) (daemon.ServeTarget, error) {
	info, err := os.Stat(arg)
	if err != nil || !info.IsDir() {
		return daemon.ServeTarget{Ref: arg}, nil
	}
	if _, err := os.Stat(filepath.Join(arg, bundle.AgentfileName)); err != nil {
		// A directory without an Agentfile still gets locate's clearer error.
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
