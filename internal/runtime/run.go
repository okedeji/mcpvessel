package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"

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
}

// Run is the end-to-end flow behind `agentcage run`. It:
//
//  1. Extracts the bundle into a temp directory.
//  2. Asks the platform provisioner to make sure containerd and
//     buildkitd are reachable (provisioning the macOS Lima VM the
//     first time it sees one).
//  3. Builds the agent's image into containerd's local image store.
//  4. Creates a container + task with the agent's stdio piped.
//  5. Speaks MCP to the agent: lists its tools, picks one based on
//     input.Tool and pickTool's rules, calls it.
//  6. Prints the tool's text response to stdout.
//  7. Tears the task and container down cleanly.
//
// Every step that allocates external state (temp dir, task, container)
// installs its own cleanup; Run returns the first error it sees but
// always runs cleanups to completion so the host is left clean.
func Run(ctx context.Context, in RunInput) error {
	if err := validateRunInput(&in); err != nil {
		return err
	}

	// 1. Extract bundle.
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
	// AgentfileSpec is the wire format; deriving back to the struct
	// would mean carrying the conversion in two places.
	af, err := agentfile.ParseFile(filepath.Join(srcDir, "Agentfile"))
	if err != nil {
		return fmt.Errorf("re-parsing bundled Agentfile: %w", err)
	}

	// 2. Provisioner up.
	provisioner, err := DefaultProvisioner()
	if err != nil {
		return err
	}
	defer func() { _ = provisioner.Close() }()
	if err := provisioner.EnsureReady(ctx, in.Stdout, in.Stderr); err != nil {
		return err
	}

	// 3. Dial daemons.
	cd, err := DialContainerd(provisioner.ContainerdAddress())
	if err != nil {
		return err
	}
	defer func() { _ = cd.Close() }()

	bk, err := DialBuildKit(ctx, provisioner.BuildKitAddress())
	if err != nil {
		return err
	}
	defer func() { _ = bk.Close() }()

	// 4. Build the image.
	imageRef := deriveImageRef(in.BundlePath)
	if err := BuildAgent(ctx, bk, BuildInput{
		Agentfile: af,
		Manifest:  manifest,
		SourceDir: srcDir,
		ImageRef:  imageRef,
	}); err != nil {
		return err
	}

	// 5. Container + task lifecycle.
	image, err := cd.Client().GetImage(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("looking up image %s: %w", imageRef, err)
	}

	runID := in.RunID
	if runID == "" {
		runID = deriveRunID(in.BundlePath, manifest.FilesHash)
	}

	container, err := cd.Client().NewContainer(ctx, runID,
		client.WithImage(image),
		client.WithNewSnapshot(runID+"-snapshot", image),
		client.WithNewSpec(oci.WithImageConfig(image)),
	)
	if err != nil {
		return fmt.Errorf("creating container %s: %w", runID, err)
	}
	defer func() {
		_ = container.Delete(context.Background(), client.WithSnapshotCleanup)
	}()

	// Stdio pipes: we write to the agent's stdin (our writes leave
	// stdinW), the agent reads from stdinR. The agent writes to
	// stdoutW, we read its responses on stdoutR. stderr passes
	// through to the operator's stderr so any agent log shows up.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	defer func() { _ = stdinW.Close() }()
	defer func() { _ = stdoutR.Close() }()

	task, err := container.NewTask(ctx, cio.NewCreator(
		cio.WithStreams(stdinR, stdoutW, in.Stderr),
	))
	if err != nil {
		return fmt.Errorf("creating task: %w", err)
	}
	defer func() {
		_, _ = task.Delete(context.Background())
	}()

	// Subscribe to the task's wait channel BEFORE starting so we never
	// race past the exit signal.
	statusCh, err := task.Wait(ctx)
	if err != nil {
		return fmt.Errorf("subscribing to task exit: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		return fmt.Errorf("starting task: %w", err)
	}

	// 6. MCP session over the agent's stdio.
	mcpClient, err := mcp.Connect(ctx, stdoutR, stdinW)
	if err != nil {
		return fmt.Errorf("MCP connect: %w", err)
	}
	defer func() { _ = mcpClient.Close() }()

	// 7. Call the tool. The CLI already resolved which one to use
	//    (run → manifest.Main; call → operator's explicit name).
	result, err := mcpClient.CallTool(ctx, in.Tool, in.Args)
	if err != nil {
		return err
	}

	// 8. Print result. Trailing newline only if the tool did not.
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	if _, err := io.WriteString(in.Stdout, result); err != nil {
		return fmt.Errorf("writing result: %w", err)
	}

	// 9. Tear down. SIGTERM is the polite request; if it does not exit
	// before the deferred Delete runs containerd will SIGKILL the
	// process for us. Either way it does not survive Run's return.
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signalling task: %w", err)
	}
	<-statusCh

	return nil
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
