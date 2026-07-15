package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/eval"
	"github.com/okedeji/mcpvessel/internal/progress"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/registry"
	"github.com/okedeji/mcpvessel/internal/resolve"
	"github.com/okedeji/mcpvessel/internal/runtime"
	"github.com/okedeji/mcpvessel/internal/store"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
	"github.com/okedeji/mcpvessel/internal/wrap"
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
		Short: "Build an agent bundle from a Vesselfile",
		Long: `Build an agent bundle from a directory containing a Vesselfile and source.

The directory defaults to the current directory. The bundle goes into the
local store under ~/.mcpvessel, addressed by its content. With -t the store also
indexes the bundle by reference, so later 'push' and 'run' commands find it by
name. Without -t the bundle is stored by content hash alone. Pass -o to also
write a portable copy.

By default build introspects the agent: it builds the image, boots the agent
briefly, and asks its MCP server for its tools, writing their descriptions,
schemas, and any private tools into the bundle's catalog. This needs the
runtime (a Linux VM on macOS) and only reads tool metadata, never running a
tool or the LLM. Pass --no-introspect to skip the agent boot and ship the
declared-only catalog (no runtime needed; USES resolution still runs).

When the Vesselfile has USES dependencies, build resolves each one's tag to a
digest and locks it into the manifest, then walks the dependency graph to
reject cycles. -t names the agent so a dependency that loops back to it is
caught; --skip-cycle-check skips the walk on a graph you trust.`,
		Example: `  mcpvessel build .
  mcpvessel build ./my-agent
  mcpvessel build . -t @okedeji/researcher:0.1
  mcpvessel build . -t @okedeji/researcher:0.1 -o researcher.agent
  mcpvessel build . --no-introspect`,
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
	// outPath is the store path, derived from the content hash before runBuild.
	outPath string
	// filePath is the -o portable copy; empty means store-only.
	filePath       string
	mode           progress.Mode
	tag            string
	skipCycleCheck bool
	noIntrospect   bool
	noCache        bool
	// env and secrets are the operator pools for the introspection boot, so a
	// server that needs a key or config to start can be introspected.
	env     map[string]string
	secrets map[string]string
}

// runBuild assembles the Build options and packages the bundle. When
// introspecting, BuildKit's output owns the screen and the packaging steps
// stay quiet; otherwise the 3-step renderer is the only progress output.
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
			// Unparseable Vesselfile; let bundle.Build report the error.
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

func buildIntoStore(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) (hash, storePath string, err error) {
	st, err := store.New()
	if err != nil {
		return "", "", err
	}

	// Stage the mcp-bridge before hashing so a hand-written agent builds without
	// the author copying a binary in themselves.
	if err := stageBridge(cfg.srcDir, stderr); err != nil {
		return "", "", err
	}

	// Anchor the hash on the store dir (outside the source tree) so it
	// matches the files_hash bundle.Build recomputes when writing the bundle.
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

	// Seed the pull cache under the OCI digest so a parent that USES this
	// agent resolves it locally, without a push.
	digest, err := registry.BundleDigest(cfg.outPath)
	if err != nil {
		return "", "", fmt.Errorf("computing bundle digest: %w", err)
	}
	if err := registry.SeedCache(digest, cfg.outPath); err != nil {
		return "", "", err
	}
	return hash, cfg.outPath, nil
}

// stageBridge stages the mcp-bridge binary into srcDir when the Vesselfile's
// ENTRYPOINT runs it, so `mcpvessel build` on a hand-written agent works
// without the author staging a linux binary by hand. import already does this
// for generated agents; this brings build to parity. A staged copy that no
// longer matches this host's companion is replaced: a stale or wrong-arch
// binary would otherwise bake into the bundle and its hash. Runs before
// hashing, so the sealed files carry the same companion the introspection
// image reuses. A Vesselfile that does not parse or does not use the bridge
// is left untouched.
func stageBridge(srcDir string, stderr io.Writer) error {
	af, err := vesselfile.ParseFile(filepath.Join(srcDir, bundle.VesselfileName))
	if err != nil || !strings.Contains(af.Entrypoint, wrap.BridgeSubcommand) {
		return nil
	}
	bin, err := runtime.FindLinuxBinary()
	if err != nil {
		return fmt.Errorf("locating the bridge binary: %w", err)
	}
	want, err := os.ReadFile(bin)
	if err != nil {
		return fmt.Errorf("reading the bridge binary %s: %w", bin, err)
	}
	dst := filepath.Join(srcDir, wrap.BridgeBinaryName)
	if have, err := os.ReadFile(dst); err == nil {
		if bytes.Equal(have, want) {
			return nil // already staged, by import or a previous build
		}
		_, _ = fmt.Fprintf(stderr, "Replaced the staged mcp-bridge in %s; it did not match this host's companion.\n", srcDir)
	} else {
		_, _ = fmt.Fprintf(stderr, "Staged the mcp-bridge into %s.\n", srcDir)
	}
	if err := os.WriteFile(dst, want, 0o755); err != nil {
		return fmt.Errorf("writing the bridge binary %s: %w", dst, err)
	}
	return nil
}

// validateEvalSuite fails the build when a declared EVAL suite escapes the
// source tree, is missing, or does not parse. A malformed Vesselfile is
// bundle.Build's to report, so a parse failure here is not ours.
func validateEvalSuite(cfg buildConfig) error {
	af, err := vesselfile.ParseFile(filepath.Join(cfg.srcDir, bundle.VesselfileName))
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

func buildToStore(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) error {
	start := time.Now()

	// Parse -t up front so a bad ref fails before the expensive image build.
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

func reportBuild(stdout io.Writer, cfg buildConfig, ref reference.Reference, hash string, elapsed time.Duration) {
	size := "?"
	if info, err := os.Stat(cfg.outPath); err == nil {
		size = humanSize(info.Size())
	}
	dur := elapsed.Round(time.Millisecond)
	if ref.Tag != "" {
		_, _ = fmt.Fprintf(stdout, "Successfully built %s (%s) in %s\n", ref.Display(), size, dur)
	} else {
		_, _ = fmt.Fprintf(stdout, "Successfully built %s (%s) in %s\n", hash, size, dur)
		_, _ = fmt.Fprintln(stdout, "Tip: run it by this hash; -t @org/name:version names it for push; -o file.agent writes a portable copy")
	}
	if cfg.filePath != "" {
		_, _ = fmt.Fprintf(stdout, "Wrote %s\n", cfg.filePath)
	}
}

// introspectionOption boots the agent and returns a Build option that
// enriches the catalog with its live tool metadata. Returns a nil option, not
// an error, on an unparseable Vesselfile; bundle.Build reports that. A boot
// or tools/list failure is fatal: an agent that will not start should not ship.
func introspectionOption(ctx context.Context, stdout, stderr io.Writer, cfg buildConfig) (bundle.Option, error) {
	af, err := vesselfile.ParseFile(filepath.Join(cfg.srcDir, bundle.VesselfileName))
	if err != nil {
		return nil, nil
	}

	// Same hash inputs as the packed manifest, so the image built here is the
	// one a later run resolves and reuses.
	hash, err := bundle.HashSource(cfg.srcDir, cfg.outPath)
	if err != nil {
		return nil, fmt.Errorf("hashing source for introspection: %w", err)
	}

	tools, err := runtime.Introspect(ctx, runtime.IntrospectInput{
		Vesselfile: af,
		SourceDir:  cfg.srcDir,
		ImageRef:   runtime.ImageRef(cfg.outPath, hash, af),
		NoCache:    cfg.noCache,
		Env:        cfg.env,
		Secrets:    cfg.secrets,
		Stdout:     stderr,
		Stderr:     stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("introspecting agent tools: %w\nif the server needs a key or config to start, supply it with --secret NAME or --env KEY=VALUE (set a secret first with 'mcpvessel secrets set NAME')", err)
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

// warnMissingDescriptions never blocks the build; private tools are exempt.
func warnMissingDescriptions(w io.Writer, af *vesselfile.Vesselfile, tools []bundle.IntrospectedTool) {
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

// usesResolverOption resolves USES dependencies to digests and returns a
// Build option that locks them into the manifest. Nil when there are no USES:
// leaf builds stay offline.
func usesResolverOption(ctx context.Context, w io.Writer, cfg buildConfig) (bundle.Option, error) {
	af, err := vesselfile.ParseFile(filepath.Join(cfg.srcDir, bundle.VesselfileName))
	if err != nil {
		// bundle.Build reports the parse error.
		return nil, nil
	}
	if len(af.Uses) == 0 {
		return nil, nil
	}

	st, err := store.New()
	if err != nil {
		return nil, err
	}
	// Tolerate a missing registry: a fully local USES graph resolves with no
	// credentials, and regErr surfaces only if a remote dependency is reached.
	reg, regErr := registry.New()

	_, _ = fmt.Fprintf(w, "Resolving %d USES dependencies\n", len(af.Uses))
	result, err := resolve.New(storeFirstResolver{store: st, reg: reg, regErr: regErr}).Resolve(ctx, af.Uses, resolve.Options{
		ParentKey:      cfg.tag,
		SkipCycleCheck: cfg.skipCycleCheck,
	})
	if err != nil {
		return nil, err
	}

	return bundle.WithUsesResolver(func(u vesselfile.Use) (string, error) {
		digest, ok := result.Digests[u.Ref+":"+u.Version]
		if !ok {
			return "", fmt.Errorf("no resolved digest for %s:%s", u.Ref, u.Version)
		}
		return digest, nil
	}), nil
}

// storeFirstResolver checks the local store before the registry, so a parent
// builds against a sibling built with -t and never pushed. The locked digest
// is the deterministic OCI digest a push produces, so it stays valid after one.
type storeFirstResolver struct {
	store  *store.Store
	reg    *registry.Client
	regErr error
}

func (r storeFirstResolver) registryClient() (*registry.Client, error) {
	if r.reg == nil {
		return nil, fmt.Errorf("dependency is not in the local store and the registry is unavailable: %w", r.regErr)
	}
	return r.reg, nil
}

func (r storeFirstResolver) Resolve(ctx context.Context, ref reference.Reference) (string, error) {
	if path, ok, err := r.store.Get(ref); err != nil {
		return "", err
	} else if ok {
		return registry.BundleDigest(path)
	}
	reg, err := r.registryClient()
	if err != nil {
		return "", err
	}
	return reg.Resolve(ctx, ref)
}

func (r storeFirstResolver) Pull(ctx context.Context, ref reference.Reference) (bundlePath, digest string, err error) {
	if path, ok, err := r.store.Get(ref); err != nil {
		return "", "", err
	} else if ok {
		d, err := registry.BundleDigest(path)
		return path, d, err
	}
	reg, err := r.registryClient()
	if err != nil {
		return "", "", err
	}
	return reg.Pull(ctx, ref)
}

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
