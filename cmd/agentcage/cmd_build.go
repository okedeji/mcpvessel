package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/progress"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/resolve"
	"github.com/okedeji/agentcage/internal/runtime"
)

func newBuildCmd() *cobra.Command {
	var outPath string
	var progressFlag string
	var tag string
	var skipCycleCheck bool
	var noIntrospect bool
	cmd := &cobra.Command{
		Use:   "build [PATH]",
		Short: "Build an agent bundle from an Agentfile",
		Long: `Build an agent bundle from a directory containing an Agentfile and source.

The directory defaults to the current directory. The output is a .agent file
named after the source directory, or after the -t reference when given, or
after -o when given.

Naming the output from -t means push finds it without an explicit path:
'build -t @okedeji/researcher:0.1' writes researcher.agent, and
'push @okedeji/researcher:0.1' looks for researcher.agent.

By default build introspects the agent: it builds the image, boots the agent
briefly, and asks its MCP server for its tools, writing their descriptions,
schemas, and any private tools into the bundle's catalog. This needs the
runtime (a Linux VM on macOS) and only reads tool metadata, never running a
tool or the LLM. Pass --no-introspect to skip the agent boot and ship the
declared-only catalog (no runtime needed; USES resolution still runs).

When the Agentfile has USES dependencies, build resolves each one's tag to a
digest and locks it into the manifest, then walks the dependency graph to
reject cycles. -t names the agent so a dependency that loops back to it is
caught; --skip-cycle-check skips the walk on a graph you trust.`,
		Example: `  agentcage build .
  agentcage build ./my-agent
  agentcage build . -t @okedeji/researcher:0.1
  agentcage build . --no-introspect`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcDir := "."
			if len(args) > 0 {
				srcDir = args[0]
			}
			if outPath == "" {
				var err error
				outPath, err = defaultOutput(srcDir, tag)
				if err != nil {
					return err
				}
			}
			return runBuild(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
				srcDir:         srcDir,
				outPath:        outPath,
				mode:           progress.ParseMode(progressFlag),
				tag:            tag,
				skipCycleCheck: skipCycleCheck,
				noIntrospect:   noIntrospect,
			})
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "output path for the .agent file")
	cmd.Flags().StringVar(&progressFlag, "progress", "auto", "set progress output (auto, plain, tty)")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "reference for the agent being built (names the output and anchors USES cycle detection)")
	cmd.Flags().BoolVar(&skipCycleCheck, "skip-cycle-check", false, "skip the transitive USES cycle walk (digests are still locked)")
	cmd.Flags().BoolVar(&noIntrospect, "no-introspect", false, "skip booting the agent to enrich the catalog (declared-only, no runtime)")
	return cmd
}

type buildConfig struct {
	srcDir         string
	outPath        string
	mode           progress.Mode
	tag            string
	skipCycleCheck bool
	noIntrospect   bool
}

// runBuild assembles the Build options (USES digest resolution, tool
// introspection) and packages the bundle.
//
// With introspection (the default) the heavy visual is BuildKit's
// Docker-style image build, rendered by runtime.Introspect to stderr; the
// lightweight parse/hash/seal steps stay quiet so the output reads like a
// docker build. With --no-introspect there is no image build, so the
// 3-step packaging renderer is the primary output, the same as before.
func runBuild(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) error {
	start := time.Now()

	var buildOpts []bundle.Option

	resolverOpt, err := usesResolverOption(ctx, stdout, cfg)
	if err != nil {
		return err
	}
	if resolverOpt != nil {
		buildOpts = append(buildOpts, resolverOpt)
	}

	introspecting := !cfg.noIntrospect
	if introspecting {
		introspectOpt, err := introspectionOption(ctx, stdout, stderr, cfg)
		if err != nil {
			return err
		}
		if introspectOpt == nil {
			// The Agentfile did not parse, so fall back to the rendered
			// packaging path and let bundle.Build report the parse error.
			introspecting = false
		} else {
			buildOpts = append(buildOpts, introspectOpt)
		}
	}

	if introspecting {
		err = bundle.Build(cfg.srcDir, cfg.outPath, buildOpts...)
	} else {
		renderer := progress.New(stdout, cfg.mode)
		opts := append(buildOpts, bundle.WithProgress(func(step, total int, msg string) {
			renderer.Step(step, total, msg)
		}))
		err = bundle.Build(cfg.srcDir, cfg.outPath, opts...)
		renderer.Done()
	}
	if err != nil {
		return err
	}

	size := "?"
	if info, statErr := os.Stat(cfg.outPath); statErr == nil {
		size = humanSize(info.Size())
	}
	_, _ = fmt.Fprintf(stdout, "Successfully built %s (%s) in %s\n",
		cfg.outPath, size, time.Since(start).Round(time.Millisecond))
	return nil
}

// introspectionOption boots the agent, reads its tools, and returns a Build
// option that enriches the catalog with descriptions, schemas, and private
// tools. It returns a nil option (not an error) when the Agentfile does not
// parse, leaving that for bundle.Build to report. A boot or tools/list
// failure is fatal: a bundle whose agent will not start should not ship.
func introspectionOption(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) (bundle.Option, error) {
	af, err := agentfile.ParseFile(filepath.Join(cfg.srcDir, bundle.AgentfileName))
	if err != nil {
		return nil, nil
	}

	// The same source files hash the packed manifest records, so the image
	// introspection builds here is the one the later run resolves and reuses.
	hash, err := bundle.HashSource(cfg.srcDir, cfg.outPath)
	if err != nil {
		return nil, fmt.Errorf("hashing source for introspection: %w", err)
	}

	tools, err := runtime.Introspect(ctx, runtime.IntrospectInput{
		Agentfile: af,
		SourceDir: cfg.srcDir,
		ImageRef:  runtime.ImageRef(cfg.outPath, hash),
		Stdout:    stderr,
		Stderr:    stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("introspecting agent tools: %w", err)
	}

	introspected := make([]bundle.IntrospectedTool, len(tools))
	for i, t := range tools {
		introspected[i] = bundle.IntrospectedTool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		}
	}
	warnMissingDescriptions(stderr, af, introspected)
	_, _ = fmt.Fprintf(stdout, "Introspected %d tools\n", len(introspected))
	return bundle.WithIntrospectedTools(introspected), nil
}

// warnMissingDescriptions nudges authors toward describing their public
// surface. A public tool with no description is what consumers and calling
// LLMs read, so its absence is worth a warning. It never blocks the build,
// and private tools are exempt.
func warnMissingDescriptions(w io.Writer, af *agentfile.Agentfile, tools []bundle.IntrospectedTool) {
	public := map[string]bool{}
	if af.Main != "" {
		public[af.Main] = true
	}
	for _, name := range af.Expose {
		public[name] = true
	}
	for _, t := range tools {
		if public[t.Name] && strings.TrimSpace(t.Description) == "" {
			_, _ = fmt.Fprintf(w, "warning: public tool %q has no description\n", t.Name)
		}
	}
}

// usesResolverOption resolves the Agentfile's USES dependencies to digests
// and returns a Build option that locks them into the manifest. It returns
// nil (no option, no network) when the agent has no USES, so a leaf build
// stays offline.
func usesResolverOption(ctx context.Context, w io.Writer, cfg buildConfig) (bundle.Option, error) {
	af, err := agentfile.ParseFile(filepath.Join(cfg.srcDir, bundle.AgentfileName))
	if err != nil {
		// A malformed Agentfile is bundle.Build's to report, not ours.
		return nil, nil
	}
	if len(af.Uses) == 0 {
		return nil, nil
	}

	reg, err := registry.New()
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(w, "Resolving %d USES dependencies\n", len(af.Uses))
	result, err := resolve.New(reg).Resolve(ctx, af.Uses, resolve.Options{
		ParentKey:      cfg.tag,
		SkipCycleCheck: cfg.skipCycleCheck,
	})
	if err != nil {
		return nil, err
	}

	return bundle.WithUsesResolver(func(u agentfile.Use) (string, error) {
		digest, ok := result.Digests[u.Ref+":"+u.Version]
		if !ok {
			return "", fmt.Errorf("no resolved digest for %s:%s", u.Ref, u.Version)
		}
		return digest, nil
	}), nil
}

// defaultOutput picks the .agent filename when -o was not given. With a
// -t reference, the name comes from the agent's name so push finds it by
// convention; otherwise it comes from the source directory's basename.
func defaultOutput(srcDir, tag string) (string, error) {
	if tag != "" {
		ref, err := reference.Parse(tag)
		if err != nil {
			return "", err
		}
		return path.Base(ref.Repository) + ".agent", nil
	}
	return defaultOutputPath(srcDir), nil
}

// defaultOutputPath derives a .agent filename from the source directory's
// basename. "." resolves to the cwd's basename so `agentcage build .` in
// /Users/x/researcher writes ./researcher.agent.
func defaultOutputPath(srcDir string) string {
	abs, err := filepath.Abs(srcDir)
	if err != nil {
		// Fall back to a generic name; the build itself will surface the
		// real error if there is one.
		return "agent.agent"
	}
	return filepath.Base(abs) + ".agent"
}

// humanSize formats n bytes in the smallest binary unit that keeps the
// number above 1, matching how Docker reports image sizes.
func humanSize(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	}
	return fmt.Sprintf("%d B", n)
}
