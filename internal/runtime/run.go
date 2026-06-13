package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/mcp"
)

// RunInput drives Run. Mostly mirrors the CLI's `agentcage run` and
// `agentcage call` flags.
type RunInput struct {
	// BundlePath is the .agent file the operator wants to run.
	BundlePath string

	// Tool is the MCP tool name to call. The CLI is responsible for
	// resolving it: `agentcage run` passes the bundle's main tool,
	// `agentcage call` passes the explicit name the operator gave.
	// Required.
	Tool string

	// Args is the MCP tools/call argument map. Marshaled to JSON by
	// the MCP client and validated against the agent's input schema.
	Args map[string]any

	// RunID names the containerd container; if empty Run derives one
	// from the bundle's hash plus a unique suffix.
	RunID string

	// Stdout / Stderr receive provisioning progress, the agent's
	// stderr stream, and the final tool result. Callers typically
	// pass os.Stdout and os.Stderr; tests can capture into a buffer.
	Stdout io.Writer
	Stderr io.Writer

	// Verbose, when true, streams the underlying provisioner output
	// (Lima's stdout/stderr on macOS) directly to Stderr instead of
	// the clean phase UI. Operators set this with `--verbose` when
	// the polite renderer is hiding something they need to see.
	Verbose bool

	// NoCache forces every image to rebuild from scratch, ignoring both an
	// already-built content-addressed image and BuildKit's layer cache.
	NoCache bool
}

// Run is the end-to-end flow behind `agentcage run`. It extracts the
// bundle, boots the agent (provision, build image, start container, open
// an MCP session via bootAgent), calls the requested tool, prints the
// result, and tears the agent down. A non-zero container exit surfaces
// through teardown.
func Run(ctx context.Context, in RunInput) error {
	if err := validateRunInput(&in); err != nil {
		return err
	}

	srcDir, err := os.MkdirTemp("", "agentcage-run-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	manifest, err := bundle.Extract(in.BundlePath, srcDir)
	if err != nil {
		return err
	}

	// Reparse the Agentfile from the materialized source so we get the
	// in-memory Agentfile struct the build path expects. The manifest's
	// AgentfileSpec is the wire format; deriving back to the struct would
	// mean carrying the conversion in two places.
	af, err := agentfile.ParseFile(filepath.Join(srcDir, "Agentfile"))
	if err != nil {
		return fmt.Errorf("re-parsing bundled Agentfile: %w", err)
	}

	runID := in.RunID
	if runID == "" {
		runID = deriveRunID(in.BundlePath, manifest.FilesHash)
	}

	boot := bootInput{
		Agentfile: af,
		Manifest:  manifest,
		SourceDir: srcDir,
		ImageRef:  deriveImageRef(in.BundlePath, manifest.FilesHash),
		RunID:     runID,
		Stdout:    in.Stdout,
		Stderr:    in.Stderr,
		Verbose:   in.Verbose,
		NoCache:   in.NoCache,
	}
	client, teardown, err := bootRun(ctx, in, boot, runID)
	if err != nil {
		return err
	}

	// The CLI already resolved which tool to call (run -> manifest.Main;
	// call -> the operator's explicit name).
	result, err := client.CallTool(ctx, in.Tool, in.Args)
	if err != nil {
		_ = teardown()
		return err
	}

	// Trailing newline only if the tool did not supply one.
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	if _, err := io.WriteString(in.Stdout, result); err != nil {
		_ = teardown()
		return fmt.Errorf("writing result: %w", err)
	}

	return teardown()
}

// bootInput carries everything bootAgent needs to build an agent's image
// and start it speaking MCP. Manifest is optional and used only for the
// image's provenance labels.
type bootInput struct {
	Agentfile *agentfile.Agentfile
	Manifest  *bundle.Manifest
	SourceDir string
	ImageRef  string
	RunID     string

	// Network and Env place the parent on a per-run container network and
	// inject its sub-agent URLs when the orchestrator wires a USES tree.
	// Both are empty for a single-container run, which keeps the parent on
	// the default network with no injected environment.
	Network string
	Env     map[string]string

	// NoCache forces every image to rebuild even when a content-addressed
	// image of the same source is already present.
	NoCache bool

	Stdout  io.Writer
	Stderr  io.Writer
	Verbose bool
}

// teardown accumulates cleanup steps and runs them in reverse on stop,
// joining every error so one failed step never strands the rest. Boot
// helpers push to it as they bring resources up, so a boot that fails
// partway still releases what it already acquired.
type teardown struct {
	steps []func() error
}

func (t *teardown) push(step func() error) { t.steps = append(t.steps, step) }

func (t *teardown) run() error {
	var errs []error
	for i := len(t.steps) - 1; i >= 0; i-- {
		if err := t.steps[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// bootSession is the provisioned runtime a boot builds and starts
// containers against: the platform provisioner and a BuildKit client. Both
// the single-container and the tree boot share it.
type bootSession struct {
	provisioner Provisioner
	bk          *BuildKit
}

// bootAgent provisions the runtime, builds the agent's image, starts its
// container, and opens an MCP session to it. It returns the connected
// client and a teardown the caller runs when finished. A boot that fails
// partway runs the teardown it accumulated before returning, so a failed
// boot leaks nothing.
func bootAgent(ctx context.Context, in bootInput) (*mcp.Client, func() error, error) {
	td := &teardown{}
	booted := false
	defer func() {
		if !booted {
			_ = td.run()
		}
	}()

	sess, err := newBootSession(ctx, in, td)
	if err != nil {
		return nil, nil, err
	}

	client, err := startAttachedAgent(ctx, sess, in, td)
	if err != nil {
		return nil, nil, err
	}

	booted = true
	return client, td.run, nil
}

// newBootSession brings up the provisioner and a BuildKit client, pushing
// their Close onto td. On first run the provisioner provisions the macOS
// Lima VM behind a phase-aware setup UI; when already ready it shows
// nothing.
func newBootSession(ctx context.Context, in bootInput, td *teardown) (*bootSession, error) {
	provisioner, err := DefaultProvisioner()
	if err != nil {
		return nil, err
	}
	td.push(provisioner.Close)
	if !SetupAlreadyReady(ctx, provisioner) {
		ui := NewSetupUI(in.Stderr)
		if err := EnsureBootstrap(ctx, provisioner, ui, in.Verbose, in.Stderr); err != nil {
			return nil, err
		}
	}

	// BuildKit's gRPC API works over the forwarded socket, which sidesteps
	// the cross-host snapshot pain container lifecycle hits.
	bk, err := DialBuildKit(ctx, provisioner.BuildKitAddress())
	if err != nil {
		return nil, err
	}
	td.push(bk.Close)

	return &bootSession{provisioner: provisioner, bk: bk}, nil
}

// buildImage builds in.ImageRef unless it is already present. Image refs are
// content-addressed, so an existing ref is provably the same source and the
// BuildKit solve is skipped; noCache forces a rebuild regardless. This is
// what keeps a repeated run from re-invoking BuildKit for an unchanged agent.
func buildImage(ctx context.Context, sess *bootSession, in BuildInput, noCache bool, stderr io.Writer) error {
	if !noCache && imageExists(ctx, sess.provisioner, in.ImageRef) {
		return nil
	}
	in.NoCache = noCache
	return buildWithProgress(ctx, sess.bk, in, stderr)
}

// startAttachedAgent builds the agent's image and starts its container
// attached over stdio, opening an MCP session the runtime drives. The
// container's stdin EOF is its signal to exit, so this is the parent the
// host speaks to; sub-agents start detached and speak HTTP instead. Its
// cleanups push onto td.
func startAttachedAgent(ctx context.Context, sess *bootSession, in bootInput, td *teardown) (*mcp.Client, error) {
	if err := buildImage(ctx, sess, BuildInput{
		Agentfile: in.Agentfile,
		Manifest:  in.Manifest,
		SourceDir: in.SourceDir,
		ImageRef:  in.ImageRef,
	}, in.NoCache, in.Stderr); err != nil {
		return nil, err
	}

	// On macOS this enters the Lima VM's rootless mount namespace via
	// limactl shell; on Linux it shells out to nerdctl directly.
	cmd := sess.provisioner.Nerdctl(ctx, nerdctlRunArgs(ContainerSpec{
		RunID:    in.RunID,
		ImageRef: in.ImageRef,
		Network:  in.Network,
		Env:      in.Env,
	})...)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = in.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting container subprocess: %w", err)
	}
	td.push(func() error {
		// Closing stdin is the agent's signal to exit; --rm then removes
		// the container and Wait reaps the subprocess.
		_ = stdinPipe.Close()
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("container subprocess exited with error: %w", err)
		}
		return nil
	})

	client, err := mcp.Connect(ctx, stdoutPipe, stdinPipe)
	if err != nil {
		// The container is live but will not get its stdin EOF through the
		// normal path; kill it so the deferred teardown's Wait reaps it.
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("MCP connect: %w", err)
	}
	td.push(client.Close)

	return client, nil
}

// validateRunInput rejects calls that cannot reach the start of the
// flow. Fills defaults that the caller did not provide.
func validateRunInput(in *RunInput) error {
	if in.BundlePath == "" {
		return fmt.Errorf("RunInput.BundlePath is required")
	}
	if in.Tool == "" {
		return fmt.Errorf("RunInput.Tool is required (CLI must resolve main or pass an explicit tool name)")
	}
	if _, err := os.Stat(in.BundlePath); err != nil {
		return fmt.Errorf("bundle %s: %w", in.BundlePath, err)
	}
	if in.Stdout == nil {
		in.Stdout = os.Stdout
	}
	if in.Stderr == nil {
		in.Stderr = os.Stderr
	}
	if in.Args == nil {
		in.Args = map[string]any{}
	}
	return nil
}

// deriveImageRef is the local containerd image ref for an agent: its name
// from the bundle basename, its tag the source files hash. Content in the
// tag means an unchanged agent resolves to the same ref (so a build is
// reused or skipped) while a changed one gets a new ref and rebuilds. It is
// not a registry ref and never pushed, so the no-latest rule that governs
// USES does not apply; the hash tag is the point.
func deriveImageRef(bundlePath, filesHash string) string {
	base := filepath.Base(bundlePath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "agent"
	}
	tag := shortDigest(filesHash)
	if tag == "" {
		tag = "build"
	}
	return "agentcage/" + sanitizeRef(base) + ":" + tag
}

// shortDigest is the first 12 hex chars of a sha256 ("sha256:abc..." -> "abc").
func shortDigest(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// deriveRunID names the containerd container for one run. Uniqueness
// across simultaneous runs comes from suffixing the bundle's content
// hash (the manifest's files_hash). Operators see this ID in
// `nerdctl ps` and trace tooling.
func deriveRunID(bundlePath, filesHash string) string {
	base := filepath.Base(bundlePath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "agent"
	}
	suffix := strings.TrimPrefix(filesHash, "sha256:")
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	if suffix == "" {
		suffix = "run"
	}
	return sanitizeRef(base) + "-" + suffix
}

// sanitizeRef converts a bundle basename into a fragment that is safe
// to use as an OCI ref component or a containerd container ID: ASCII
// letters, digits, dot, dash, underscore.
func sanitizeRef(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "agent"
	}
	return b.String()
}
