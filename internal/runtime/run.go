package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/mcp"
)

// RunInput drives Acquire. Mostly mirrors the CLI's `agentcage run` and
// `agentcage call` flags.
type RunInput struct {
	// BundlePath is the .agent file the operator wants to run.
	BundlePath string

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

	// Managed marks this as a daemon-managed run, labeling its containers and
	// networks so a restarted daemon can sweep a crashed predecessor's orphans.
	// The one-shot CLI leaves it false.
	Managed bool

	// Interaction is the run's loopback mode, injected into the root agent as
	// AGENTCAGE_INTERACTION: oneshot for run/call (no follow-up), interactive
	// for a held or served run. Empty injects nothing.
	Interaction string

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

// Session is one booted run the caller holds: the root agent's open MCP session,
// the working set that owns the run's cages and releases the whole graph
// (containers, networks, gateways), and the run id that names it. A one-shot
// run/call acquires one, calls once, and releases; a held or served run keeps it
// across many calls, releasing on stop or daemon shutdown.
type Session struct {
	runID string
	root  *mcp.Client
	ws    *workingSet
}

// RunID is the run's id: the daemon's registry key and what `agentcage stop`
// names.
func (s *Session) RunID() string { return s.runID }

// Warnings are the boot-time notes the operator should see. A one-shot run
// streams them on stderr already; serve reads them here because its boot's
// stderr is the daemon log, not the operator's terminal.
func (s *Session) Warnings() []string {
	if s.ws == nil {
		return nil
	}
	return s.ws.warnings
}

// Call dispatches one tool call on the root agent's session. A one-shot run/call
// makes exactly one; a held run serves many across its lifetime.
func (s *Session) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	return s.root.CallTool(ctx, tool, args)
}

// ListTools returns the tools the held agent advertises, descriptions and input
// schemas included. The serve front door reads them to publish a filtered
// tools/list, so an external caller sees only the agent's public tools.
func (s *Session) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	return s.root.ListTools(ctx)
}

// StartWorkingSet starts the run's on-demand activation, the supervisor that
// boots inactive sub-agents as the tree calls them. The caller owns ctx: a held
// run passes a background context so activation outlives the request that booted
// it, a one-shot the request context so it ends with the call. A single-cage
// run has no tree and starts nothing. Release cancels whatever this starts.
func (s *Session) StartWorkingSet(ctx context.Context) {
	s.ws.start(ctx)
}

// Release tears the run down. The working set stops activation, then joins every
// cleanup step's error, so a non-zero container exit or a failed network removal
// surfaces here.
func (s *Session) Release() error {
	return s.ws.releaseAll()
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
		Agentfile:   af,
		Manifest:    manifest,
		SourceDir:   srcDir,
		ImageRef:    deriveImageRef(in.BundlePath, manifest.FilesHash),
		RunID:       runID,
		Budget:      in.Budget,
		OpEnv:       in.Env,
		OpSecrets:   in.Secrets,
		Stdout:      in.Stdout,
		Stderr:      in.Stderr,
		Verbose:     in.Verbose,
		NoCache:     in.NoCache,
		Managed:     in.Managed,
		Interaction: in.Interaction,
	}
	client, ws, err := bootRun(ctx, in, boot, runID)
	if err != nil {
		return nil, err
	}
	return &Session{runID: runID, root: client, ws: ws}, nil
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
	// Both are empty for a single-cage run, which keeps the parent on
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

	// MaxLive, HostMax, and IdleTTL are the resolved cage policy the working set
	// enforces: the per-run and host live-cage caps and the idle reap threshold.
	// Set by bootRun from the operator's config.
	MaxLive int
	HostMax int
	IdleTTL time.Duration

	// MachineMemCap is the operator's machine.memory_gib in bytes, 0 when unset.
	// It caps the memory the run admits against; a value above the machine's real
	// memory is ignored and flagged so the operator is told to recreate the VM.
	MachineMemCap int64

	// OpEnv and OpSecrets are the operator's value pools, injected into an
	// agent only for the names it declares.
	OpEnv     map[string]string
	OpSecrets map[string]string

	// NoCache forces every image to rebuild even when a content-addressed
	// image of the same source is already present.
	NoCache bool

	// Managed labels this run's containers and networks as daemon-managed so a
	// restarted daemon can sweep a crashed predecessor's orphans.
	Managed bool

	// Interaction injects AGENTCAGE_INTERACTION into the root agent so its LLM
	// knows whether a follow-up turn is possible. Empty injects nothing.
	Interaction string

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
// the single-cage and the tree boot share it.
type bootSession struct {
	provisioner Provisioner
	bk          *BuildKit
}

// bootAgent provisions the runtime, builds the agent's image, starts its
// container, and opens an MCP session to it. It returns the connected
// client and a teardown the caller runs when finished. A boot that fails
// partway runs the teardown it accumulated before returning, so a failed
// boot leaks nothing.
func bootAgent(ctx context.Context, in bootInput) (*mcp.Client, *workingSet, error) {
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

	// Refuse the run before starting anything if the agent's cage (plus its
	// gateways) does not fit the machine, the same admission a USES tree gets.
	// in.Cap is the operator's effective cap; a caller that bypassed bootRun
	// (introspection) leaves it empty, so fall back to the runtime default.
	rootMem := in.Cap.MemBytes()
	if rootMem == 0 {
		rootMem = defaultAgentCap.MemBytes()
	}
	usable, err := usableMemory(sess.provisioner, in.MachineMemCap, in.Stderr)
	if err != nil {
		return nil, nil, err
	}
	if need := soloBaselineMemory(rootMem, model != "", len(allowHosts) > 0); need > usable {
		return nil, nil, fmt.Errorf("this agent needs %s but the machine has %s usable: lower its RESOURCES cap or use a machine with more memory",
			HumanBytes(need), HumanBytes(usable))
	}

	egressNet := in.RunID + "-egress"
	if model != "" || len(allowHosts) > 0 {
		network := in.RunID + "-net"
		if err := createNetwork(ctx, sess.provisioner, network, true, in.Managed); err != nil {
			return nil, nil, err
		}
		td.push(func() error { return removeNetwork(sess.provisioner, network) })

		if err := createNetwork(ctx, sess.provisioner, egressNet, false, in.Managed); err != nil {
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
	return client, &workingSet{sess: sess, td: td}, nil
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

	// Only the attached root carries the interaction mode: it is the agent whose
	// turn ends when the run does. Sub-agents always have their parent as a live
	// caller, so the question never reaches them.
	if in.Interaction != "" {
		if in.Env == nil {
			in.Env = map[string]string{}
		}
		in.Env[env.Interaction] = in.Interaction
	}

	// On macOS this enters the Lima VM's rootless mount namespace via
	// limactl shell; on Linux it shells out to nerdctl directly.
	cmd := sess.provisioner.Nerdctl(ctx, nerdctlRunArgs(ContainerSpec{
		RunID:    in.RunID,
		ImageRef: in.ImageRef,
		Networks: []string{in.Network},
		Env:      in.Env,
		Managed:  in.Managed,
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
