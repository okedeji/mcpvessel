package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/eval"
	"github.com/okedeji/agentcage/internal/progress"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/resolve"
	"github.com/okedeji/agentcage/internal/runtime"
	"github.com/okedeji/agentcage/internal/store"
)

func newBuildCmd() *cobra.Command {
	var outPath string
	var progressFlag string
	var tag string
	var skipCycleCheck bool
	var noIntrospect bool
	var noCache bool
	cmd := &cobra.Command{
		Use:   "build [PATH]",
		Short: "Build an agent bundle from an Agentfile",
		Long: `Build an agent bundle from a directory containing an Agentfile and source.

The directory defaults to the current directory. The bundle goes into the
local store under ~/.agentcage, addressed by its content. With -t the store also 
indexes the bundle by reference, so 'build -t @okedeji/researcher:0.1' lets 
'push @okedeji/researcher:0.1' and 'run @okedeji/researcher:0.1' find it by name 
with no file to line up. Without -t the bundle is stored by content hash alone. 
Pass -o to also write a portable copy you can move by hand.

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
  agentcage build . -t @okedeji/researcher:0.1 -o researcher.agent
  agentcage build . --no-introspect`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcDir := "."
			if len(args) > 0 {
				srcDir = args[0]
			}
			return buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
				srcDir:         srcDir,
				filePath:       outPath,
				mode:           progress.ParseMode(progressFlag),
				tag:            tag,
				skipCycleCheck: skipCycleCheck,
				noIntrospect:   noIntrospect,
				noCache:        noCache,
			})
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "also write a portable copy of the bundle to this path")
	cmd.Flags().StringVar(&progressFlag, "progress", "auto", "set progress output (auto, plain, tty)")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "reference for the agent being built (names the output and anchors USES cycle detection)")
	cmd.Flags().BoolVar(&skipCycleCheck, "skip-cycle-check", false, "skip the transitive USES cycle walk (digests are still locked)")
	cmd.Flags().BoolVar(&noIntrospect, "no-introspect", false, "skip booting the agent to enrich the catalog (declared-only, no runtime)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "rebuild the introspection image from scratch, ignoring cached and already-built images")
	return cmd
}

type buildConfig struct {
	srcDir string
	// outPath is the store path build writes the bundle to. buildToStore
	// derives it from the content hash and sets it before calling runBuild.
	outPath string
	// filePath is the -o value: an optional portable copy written after the
	// bundle lands in the store. Empty leaves the store as the only output.
	filePath       string
	mode           progress.Mode
	tag            string
	skipCycleCheck bool
	noIntrospect   bool
	noCache        bool
}

// runBuild assembles the Build options (USES digest resolution, tool
// introspection) and packages the bundle.
//
// With introspection (the default) the heavy visual is BuildKit's image
// build, rendered by runtime.Introspect to stderr; the lightweight
// parse/hash/seal steps stay quiet so the build output stays the primary
// thing on screen. With --no-introspect there is no image build, so the
// 3-step packaging renderer is the primary output, the same as before.
func runBuild(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) error {
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
	return err
}

// buildIntoStore hashes the source and builds the bundle into the content-
// addressed store, returning the content hash and the store path it wrote. It
// is the core buildToStore reports on, and the seam `agentcage eval .` reuses
// to make a source directory runnable before evaluating it.
func buildIntoStore(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) (hash, storePath string, err error) {
	st, err := store.New()
	if err != nil {
		return "", "", err
	}

	// Anchor the hash on the store dir, which sits outside the source tree:
	// it excludes nothing from the walk, so this value matches the files_hash
	// bundle.Build recomputes when it writes into the store, and the store
	// path lands where a later run resolves the same source to.
	hash, err = bundle.HashSource(cfg.srcDir, st.Dir())
	if err != nil {
		return "", "", err
	}
	cfg.outPath = st.PathFor(hash)

	if err := validateEvalSuite(cfg); err != nil {
		return "", "", err
	}

	if err := runBuild(ctx, stdout, stderr, cfg); err != nil {
		return "", "", err
	}
	return hash, cfg.outPath, nil
}

// validateEvalSuite fails the build when the Agentfile declares an EVAL suite
// that escapes the source tree, is missing, or does not parse. This is the
// domain check the parser defers: the parser takes the raw path on faith; the
// build confirms it points at a real, well-formed suite before the bundle ships
// claiming one. A malformed Agentfile is bundle.Build's to report, so a parse
// failure here is not ours.
func validateEvalSuite(cfg buildConfig) error {
	af, err := agentfile.ParseFile(filepath.Join(cfg.srcDir, bundle.AgentfileName))
	if err != nil || af.Eval == "" {
		return nil
	}
	rel := filepath.Clean(af.Eval)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("EVAL %s: path escapes the source directory", af.Eval)
	}
	if _, err := eval.LoadSuiteFile(filepath.Join(cfg.srcDir, rel)); err != nil {
		return fmt.Errorf("EVAL %s: %w", af.Eval, err)
	}
	return nil
}

// buildToStore builds the bundle into the content-addressed store, indexes it
// by ref when -t is given, writes the -o copy when asked, and reports the
// result. The store is the build's output; push, run, and call read it back by
// ref.
func buildToStore(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) error {
	start := time.Now()

	// Parse the ref before the build so a bad -t fails fast, not after the
	// expensive image build.
	var ref reference.Reference
	if cfg.tag != "" {
		var err error
		ref, err = reference.Parse(cfg.tag)
		if err != nil {
			return err
		}
	}

	hash, storePath, err := buildIntoStore(ctx, stdout, stderr, cfg)
	if err != nil {
		return err
	}
	cfg.outPath = storePath

	if ref.Tag != "" {
		st, err := store.New()
		if err != nil {
			return err
		}
		if err := st.Tag(ref, hash); err != nil {
			return err
		}
	}
	if cfg.filePath != "" {
		if err := store.CopyTo(cfg.outPath, cfg.filePath); err != nil {
			return err
		}
	}

	reportBuild(stdout, cfg, ref, hash, time.Since(start))
	return nil
}

// reportBuild prints the one-line build result: the ref when the bundle was
// named with -t, otherwise its content hash plus a hint at how to name it. The
// -o copy, when written, gets its own line.
func reportBuild(stdout io.Writer, cfg buildConfig, ref reference.Reference, hash string, elapsed time.Duration) {
	size := "?"
	if info, err := os.Stat(cfg.outPath); err == nil {
		size = humanSize(info.Size())
	}
	dur := elapsed.Round(time.Millisecond)
	if ref.Tag != "" {
		_, _ = fmt.Fprintf(stdout, "Successfully built %s (%s) in %s\n", ref.OCIRef(), size, dur)
	} else {
		_, _ = fmt.Fprintf(stdout, "Successfully built %s (%s) in %s\n", hash, size, dur)
		_, _ = fmt.Fprintln(stdout, "Tip: run it by this hash; -t @org/name:version names it for push; -o file.agent writes a portable copy")
	}
	if cfg.filePath != "" {
		_, _ = fmt.Fprintf(stdout, "Wrote %s\n", cfg.filePath)
	}
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
		NoCache:   cfg.noCache,
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

// humanSize formats n bytes in the smallest binary unit that keeps the
// number above 1, the conventional way image sizes are shown.
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
