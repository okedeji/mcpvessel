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
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
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

	// Budget is the operator's concrete cap on the run's LLM spend, in
	// micro-USD. It overrides the agent's advisory BUDGET; 0 means the
	// operator set none and the advisory (or unbounded) applies.
	Budget int64

	// Env and Secrets are the operator's value pools for the run. The runtime
	// injects into each agent only the names that agent's manifest declares,
	// so a value is never visible to an agent that did not ask for it.
	Env     map[string]string
	Secrets map[string]string

	// Resources is the operator's --memory/--cpus/--pids cap for this run. It
	// overrides the configured default cap per field; a per-agent config cap
	// still wins, since it is the more specific choice. Empty fields leave the
	// configured (then runtime) default in place.
	Resources config.Cap

	// RunID names the containerd container; if empty Run derives one
	// from Name plus a unique suffix.
	RunID string

	// Name is the friendly base the derived run id reads as, the agent's repo
	// or file name (e.g. "echo") rather than the store's content-hash filename.
	// Empty falls back to the bundle path's basename.
	Name string

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

// Run is the one-shot flow behind `agentcage run` and `agentcage call`: acquire
// a booted run, dispatch the single tool call, print the result, release the
// run. acquire, call, and release are split out so the daemon and warm pool can
// later hold a run open across many calls; the one-shot path releases after one.
// A non-zero container exit surfaces through release.
func Run(ctx context.Context, in RunInput) error {
	if err := validateRunInput(&in); err != nil {
		return err
	}

	s, err := Acquire(ctx, in)
	if err != nil {
		return err
	}

	// The CLI already resolved which tool to call (run -> manifest.Main;
	// call -> the operator's explicit name).
	result, err := s.Call(ctx, in.Tool, in.Args)
	if err != nil {
		_ = s.Release()
		return err
	}

	// Trailing newline only if the tool did not supply one.
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	if _, err := io.WriteString(in.Stdout, result); err != nil {
		_ = s.Release()
		return fmt.Errorf("writing result: %w", err)
	}

	return s.Release()
}

// Session is one booted run the caller holds: the root agent's open MCP session,
// the teardown that releases the whole graph (containers, networks, gateways),
// and the run id that names it. The one-shot Run acquires one, calls once, and
// releases; the daemon holds many across their lifetime, dispatching a call per
// request and releasing on stop.
type Session struct {
	runID    string
	root     *mcp.Client
	teardown func() error
}

// RunID is the run's id: the daemon's registry key and what `agentcage stop`
// names.
func (s *Session) RunID() string { return s.runID }

// Call dispatches one tool call on the root agent's session. The one-shot Run
// makes exactly one; a held run serves many across its lifetime.
func (s *Session) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	return s.root.CallTool(ctx, tool, args)
}

// Release tears the run down. The teardown joins every cleanup step's error, so
// a non-zero container exit or a failed network removal surfaces here.
func (s *Session) Release() error {
	return s.teardown()
}

// Acquire extracts the bundle, boots the run (provision, build the image, start
// the container graph, open the root's MCP session over stdio), and returns a
// held Session. The extracted source is only read while the image builds, so it
// is removed before returning rather than held for the run's lifetime. A boot
// that fails partway releases what it acquired inside bootRun, so Acquire leaks
// nothing on error.
func Acquire(ctx context.Context, in RunInput) (*Session, error) {
	srcDir, err := os.MkdirTemp("", "agentcage-run-*")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	manifest, err := bundle.Extract(in.BundlePath, srcDir)
	if err != nil {
		return nil, err
	}

	// Reparse the Agentfile from the materialized source so we get the
	// in-memory Agentfile struct the build path expects. The manifest's
	// AgentfileSpec is the wire format; deriving back to the struct would
	// mean carrying the conversion in two places.
	af, err := agentfile.ParseFile(filepath.Join(srcDir, "Agentfile"))
	if err != nil {
		return nil, fmt.Errorf("re-parsing bundled Agentfile: %w", err)
	}

	runID := in.RunID
	if runID == "" {
		name := in.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(in.BundlePath), filepath.Ext(in.BundlePath))
		}
		runID = deriveRunID(name, manifest.FilesHash)
	}

	boot := bootInput{
		Agentfile: af,
		Manifest:  manifest,
		SourceDir: srcDir,
		ImageRef:  deriveImageRef(in.BundlePath, manifest.FilesHash),
		RunID:     runID,
		Budget:    in.Budget,
		OpEnv:     in.Env,
		OpSecrets: in.Secrets,
		Stdout:    in.Stdout,
		Stderr:    in.Stderr,
		Verbose:   in.Verbose,
		NoCache:   in.NoCache,
	}
	client, teardown, err := bootRun(ctx, in, boot, runID)
	if err != nil {
		return nil, err
	}
	return &Session{runID: runID, root: client, teardown: teardown}, nil
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

	// Budget is the operator's --budget override in micro-USD; 0 falls back
	// to the agent's advisory BUDGET.
	Budget int64

	// Cap is the attached agent's resolved resource cap, already through the
	// operator's config. Empty only when a caller bypasses bootRun, in which
	// case startAttachedAgent falls back to the runtime default rather than
	// running the agent uncapped.
	Cap config.Cap

	// OpEnv and OpSecrets are the operator's value pools, injected into an
	// agent only for the names it declares.
	OpEnv     map[string]string
	OpSecrets map[string]string

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

	// A lone agent with no USES still needs a per-run network when it reasons
	// (a MODEL, so an LLM gateway to reach) or declares allow: egress (an
	// egress proxy to route through). Either way the run network is internal
	// and the gateways are its only doors out. Without either it stays on the
	// default network, unchanged.
	model := manifestModel(in.Manifest)
	allowHosts := egressHosts(manifestEgress(in.Manifest))
	egressNet := in.RunID + "-egress"
	if model != "" || len(allowHosts) > 0 {
		network := in.RunID + "-net"
		if err := createNetwork(ctx, sess.provisioner, network, true); err != nil {
			return nil, nil, err
		}
		td.push(func() error { return removeNetwork(sess.provisioner, network) })

		if err := createNetwork(ctx, sess.provisioner, egressNet, false); err != nil {
			return nil, nil, err
		}
		td.push(func() error { return removeNetwork(sess.provisioner, egressNet) })

		in.Network = network
		if in.Env == nil {
			in.Env = map[string]string{}
		}

		if model != "" {
			token, err := capabilityToken()
			if err != nil {
				return nil, nil, err
			}
			budget := resolveBudget(in.Budget, manifestBudget(in.Manifest), in.Stderr)
			llmCfg, err := buildLLMConfig(map[string]string{rootAgentKey: model}, map[string]string{rootAgentKey: token}, budget)
			if err != nil {
				return nil, nil, err
			}
			if err := startLLMGateway(ctx, sess, in.RunID, []string{network}, egressNet, llmCfg, in, td); err != nil {
				return nil, nil, err
			}
			in.Env[env.LLMURL] = llmURL(in.RunID, token)
		}

		if len(allowHosts) > 0 {
			for k, v := range egressProxyEnv(in.RunID) {
				in.Env[k] = v
			}
		}
	}

	// Operator env overrides and declared secrets, scoped to this agent's own
	// declarations. Runs for a tool collection too, which may declare secrets
	// without reasoning.
	if in.Env == nil {
		in.Env = map[string]string{}
	}
	if err := injectOperatorValues(in.Env, in.Manifest, in.OpEnv, in.OpSecrets); err != nil {
		return nil, nil, fmt.Errorf("agent %s: %w", rootAgentKey, err)
	}

	client, err := startAttachedAgent(ctx, sess, in, td)
	if err != nil {
		return nil, nil, err
	}

	// The egress proxy keys its allow-list by the agent's address, available
	// only once the container is running, so it starts after the agent.
	if len(allowHosts) > 0 {
		if err := startEgressProxy(ctx, sess, in.RunID, egressNet, map[string]egressAgent{in.RunID: {Network: in.Network, Hosts: allowHosts}}, in, td); err != nil {
			return nil, nil, err
		}
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

	// A caller that bypassed bootRun leaves Cap empty; fall back to the runtime
	// default so the root is never started uncapped.
	cap := in.Cap
	if cap == (config.Cap{}) {
		cap = defaultAgentCap
	}

	// On macOS this enters the Lima VM's rootless mount namespace via
	// limactl shell; on Linux it shells out to nerdctl directly.
	cmd := sess.provisioner.Nerdctl(ctx, nerdctlRunArgs(ContainerSpec{
		RunID:    in.RunID,
		ImageRef: in.ImageRef,
		Networks: []string{in.Network},
		Env:      in.Env,
	}.withCap(cap))...)
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

// deriveRunID names the containerd container for one run: the agent's friendly
// name plus a suffix of its content hash for uniqueness across simultaneous
// runs. Operators see this ID in `agentcage ps`, `stop`, and trace tooling, so
// name is the repo or file basename ("echo"), not the store's content-hash
// filename.
func deriveRunID(name, filesHash string) string {
	if name == "" {
		name = "agent"
	}
	suffix := shortDigest(filesHash)
	if suffix == "" {
		suffix = "run"
	}
	return sanitizeRef(name) + "-" + suffix
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
