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
	// the SDK and validated against the agent's input schema.
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

	client, teardown, err := bootAgent(ctx, bootInput{
		Agentfile: af,
		Manifest:  manifest,
		SourceDir: srcDir,
		ImageRef:  deriveImageRef(in.BundlePath),
		RunID:     runID,
		Stdout:    in.Stdout,
		Stderr:    in.Stderr,
		Verbose:   in.Verbose,
	})
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

	Stdout  io.Writer
	Stderr  io.Writer
	Verbose bool
}

// bootAgent provisions the runtime, builds the agent's image, starts its
// container, and opens an MCP session to it. It returns the connected
// client and a teardown closure the caller runs when finished.
//
// teardown reverses the boot: it closes the MCP session (the agent's
// signal to exit), waits for the container, then releases BuildKit and the
// provisioner, joining any errors. A non-zero container exit surfaces
// here. If boot fails partway, bootAgent runs the teardown it has
// accumulated so far before returning, so a failed boot leaks nothing.
func bootAgent(ctx context.Context, in bootInput) (*mcp.Client, func() error, error) {
	var cleanups []func() error
	teardown := func() error {
		var errs []error
		for i := len(cleanups) - 1; i >= 0; i-- {
			if err := cleanups[i](); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	booted := false
	defer func() {
		if !booted {
			_ = teardown()
		}
	}()

	// Provisioner up. On first run this provisions the macOS Lima VM
	// behind a phase-aware setup UI; when already ready it shows nothing.
	provisioner, err := DefaultProvisioner()
	if err != nil {
		return nil, nil, err
	}
	cleanups = append(cleanups, provisioner.Close)
	if !SetupAlreadyReady(ctx, provisioner) {
		ui := NewSetupUI(in.Stderr)
		if err := EnsureBootstrap(ctx, provisioner, ui, in.Verbose, in.Stderr); err != nil {
			return nil, nil, err
		}
	}

	// Build the image via BuildKit's gRPC API over the forwarded socket,
	// which sidesteps the cross-host snapshot pain container lifecycle hits.
	bk, err := DialBuildKit(ctx, provisioner.BuildKitAddress())
	if err != nil {
		return nil, nil, err
	}
	cleanups = append(cleanups, bk.Close)
	if err := buildWithProgress(ctx, bk, BuildInput{
		Agentfile: in.Agentfile,
		Manifest:  in.Manifest,
		SourceDir: in.SourceDir,
		ImageRef:  in.ImageRef,
	}, in.Stderr); err != nil {
		return nil, nil, err
	}

	// Start the container. On macOS this enters the Lima VM's rootless
	// mount namespace via limactl shell; on Linux it shells out to nerdctl
	// directly.
	cmd := provisioner.PrepareRunContainer(ctx, ContainerSpec{
		RunID:    in.RunID,
		ImageRef: in.ImageRef,
		Network:  in.Network,
		Env:      in.Env,
	})
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = in.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting container subprocess: %w", err)
	}
	cleanups = append(cleanups, func() error {
		// Closing stdin is the agent's signal to exit; --rm then removes
		// the container and Wait reaps the subprocess.
		_ = stdinPipe.Close()
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("container subprocess exited with error: %w", err)
		}
		return nil
	})

	// MCP session over the container's stdio.
	client, err := mcp.Connect(ctx, stdoutPipe, stdinPipe)
	if err != nil {
		// The container is live but will not get its stdin EOF through the
		// normal path; kill it so the deferred teardown's Wait reaps it.
		_ = cmd.Process.Kill()
		return nil, nil, fmt.Errorf("MCP connect: %w", err)
	}
	cleanups = append(cleanups, client.Close)

	booted = true
	return client, teardown, nil
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

// deriveImageRef turns a bundle path into the OCI image tag agentcage
// uses inside containerd's local image store. Stable across builds of
// the same agent (same basename → same ref), so re-running an agent
// reuses the BuildKit cache.
func deriveImageRef(bundlePath string) string {
	base := filepath.Base(bundlePath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "agent"
	}
	return "agentcage/" + sanitizeRef(base) + ":latest"
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
