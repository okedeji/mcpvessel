package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/progress"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/resolve"
)

func newBuildCmd() *cobra.Command {
	var outPath string
	var progressFlag string
	var tag string
	var skipCycleCheck bool
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

When the Agentfile has USES dependencies, build resolves each one's tag to a
digest and locks it into the manifest, then walks the dependency graph to
reject cycles. Resolution reaches the registry, so the dependencies must be
pushed first. -t names the agent so a dependency that loops back to it is
caught; --skip-cycle-check skips the walk on a graph you trust.`,
		Example: `  agentcage build .
  agentcage build ./my-agent
  agentcage build . -t @okedeji/researcher:0.1
  agentcage build . -o my-agent.agent`,
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
			return runBuild(cmd.Context(), cmd.OutOrStdout(), buildConfig{
				srcDir:         srcDir,
				outPath:        outPath,
				mode:           progress.ParseMode(progressFlag),
				tag:            tag,
				skipCycleCheck: skipCycleCheck,
			})
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "output path for the .agent file")
	cmd.Flags().StringVar(&progressFlag, "progress", "auto", "set progress output (auto, plain, tty)")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "reference for the agent being built (names the output and anchors USES cycle detection)")
	cmd.Flags().BoolVar(&skipCycleCheck, "skip-cycle-check", false, "skip the transitive USES cycle walk (digests are still locked)")
	return cmd
}

type buildConfig struct {
	srcDir         string
	outPath        string
	mode           progress.Mode
	tag            string
	skipCycleCheck bool
}

// runBuild calls bundle.Build with a progress renderer chosen by mode.
//
// Plain mode mirrors Docker's classic builder format (one line per
// step start, no live updates). TTY mode mirrors modern BuildKit
// output, refreshing in place with live timers. Auto picks based on
// whether w is a real terminal.
//
// When the Agentfile declares USES dependencies, build resolves their
// digests against the registry first so the manifest ships as a lockfile.
// An agent with no USES never touches the network, so a local build of a
// leaf agent stays offline.
func runBuild(ctx context.Context, w io.Writer, cfg buildConfig) error {
	start := time.Now()
	renderer := progress.New(w, cfg.mode)

	buildOpts := []bundle.Option{bundle.WithProgress(func(step, total int, msg string) {
		renderer.Step(step, total, msg)
	})}

	resolverOpt, err := usesResolverOption(ctx, w, cfg)
	if err != nil {
		return err
	}
	if resolverOpt != nil {
		buildOpts = append(buildOpts, resolverOpt)
	}

	err = bundle.Build(cfg.srcDir, cfg.outPath, buildOpts...)
	renderer.Done()
	if err != nil {
		return err
	}

	size := "?"
	if info, statErr := os.Stat(cfg.outPath); statErr == nil {
		size = humanSize(info.Size())
	}
	_, _ = fmt.Fprintf(w, "Successfully built %s (%s) in %s\n",
		cfg.outPath, size, time.Since(start).Round(time.Millisecond))
	return nil
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
