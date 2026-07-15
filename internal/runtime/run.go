package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/mcp"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
)

// RunInput drives Acquire; it mirrors the CLI's run and call flags.
type RunInput struct {
	BundlePath string

	// Budget caps LLM spend in micro-USD; 0 defers to the agent's advisory BUDGET.
	Budget int64

	// Env and Secrets are operator value pools; each agent receives only the
	// names its manifest declares.
	Env     map[string]string
	Secrets map[string]string

	// Resources overrides the default cap per field; a per-agent config cap
	// still wins.
	Resources config.Cap

	// RunID names the container; empty derives one from Name plus a unique suffix.
	RunID string

	// Name is the friendly base for a derived run id; empty uses the bundle basename.
	Name string

	// Managed labels containers and networks for the daemon's orphan sweep.
	Managed bool

	// Interaction is injected as VESSEL_INTERACTION; empty injects nothing.
	Interaction string

	// OnEvent observes the run's lifecycle events. Nil off the daemon path.
	OnEvent func(Event)

	// Record enables full-payload LLM capture for replay; heavy, off by default.
	Record bool

	Stdout io.Writer
	Stderr io.Writer

	// Verbose streams raw provisioner output to Stderr instead of the phase UI.
	Verbose bool

	// NoCache forces rebuilds, bypassing content-addressed images and
	// BuildKit's layer cache.
	NoCache bool

	// LogFile opens the run's durable log once the run id is known; the agent's
	// stderr is teed there, build progress is not. Nil logs to Stderr alone.
	LogFile func(runID string) io.WriteCloser

	// ObserveEgress runs the egress proxy in audit mode: every host is allowed
	// and recorded, so a run can learn a server's egress profile before it is
	// locked down. Off leaves the cage deny-default.
	ObserveEgress bool

	// EgressAllow is the operator's per-run egress override for the root agent:
	// hosts allowed on top of what the Vesselfile declares, for this run only.
	// It does not change the bundle; --save persists it instead.
	EgressAllow []string
}

// Session is one booted run: the root agent's open MCP session and the working
// set that owns the run's cages. Release tears down the whole graph.
type Session struct {
	runID string
	root  *mcp.Client
	ws    *workingSet

	// elicit routes the root's mid-call questions to the operator; nil (any
	// non-interactive boot) advertises no question channel.
	elicit *elicitRouter
}

// RunID returns the run's id, the daemon's registry key.
func (s *Session) RunID() string { return s.runID }

// Warnings returns the boot-time notes the operator should see.
func (s *Session) Warnings() []string {
	if s.ws == nil {
		return nil
	}
	return s.ws.warnings
}

// Call dispatches one tool call on the root agent's session.
func (s *Session) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	return s.root.CallTool(ctx, tool, args)
}

// BindElicit installs target as the operator's answer channel for one call and
// returns a release. With no router it returns a no-op.
func (s *Session) BindElicit(target mcp.ElicitHandler) func() {
	if s.elicit == nil {
		return func() {}
	}
	return s.elicit.bind(target)
}

// ListTools returns the tools the held agent advertises.
func (s *Session) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	return s.root.ListTools(ctx)
}

// StartWorkingSet starts on-demand activation of inactive sub-agents. The
// caller owns ctx; Release cancels whatever this starts. A single-cage run
// starts nothing.
func (s *Session) StartWorkingSet(ctx context.Context) {
	s.ws.start(ctx)
}

// Release tears the run down, joining every cleanup step's error.
func (s *Session) Release() error {
	return s.ws.releaseAll()
}

// Acquire extracts the bundle, boots the run, and returns a held Session. The
// extracted source is removed before returning; a boot that fails partway
// releases what it acquired, so Acquire leaks nothing on error.
func Acquire(ctx context.Context, in RunInput) (*Session, error) {
	srcDir, err := os.MkdirTemp("", "mcpvessel-run-*")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	manifest, err := bundle.Extract(in.BundlePath, srcDir)
	if err != nil {
		return nil, err
	}

	// The manifest carries only the wire-format spec; reparse the Vesselfile
	// struct the build path expects.
	af, err := vesselfile.ParseFile(filepath.Join(srcDir, "Vesselfile"))
	if err != nil {
		return nil, fmt.Errorf("re-parsing bundled Vesselfile: %w", err)
	}

	runID := in.RunID
	if runID == "" {
		name := in.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(in.BundlePath), filepath.Ext(in.BundlePath))
		}
		// The suffix keeps repeated and concurrent runs of one bundle distinct.
		runID = deriveRunID(name, manifest.FilesHash) + "-" + uniqueSuffix()
	}

	// Only an interactive boot gets a question channel; a one-shot boot never
	// advertises elicitation, so an agent that tries to ask fails closed.
	var router *elicitRouter
	if in.Interaction == env.InteractionInteractive {
		router = newElicitRouter()
		router.onEvent = in.OnEvent
		router.runID = runID
	}

	boot := bootInput{
		Vesselfile:    af,
		Manifest:      manifest,
		SourceDir:     srcDir,
		ImageRef:      deriveImageRef(in.BundlePath, manifest),
		InjectBridge:  manifestUsesBridge(manifest),
		RunID:         runID,
		Budget:        in.Budget,
		OpEnv:         in.Env,
		OpSecrets:     in.Secrets,
		Stdout:        in.Stdout,
		Stderr:        in.Stderr,
		Verbose:       in.Verbose,
		NoCache:       in.NoCache,
		Managed:       in.Managed,
		Interaction:   in.Interaction,
		OnEvent:       in.OnEvent,
		Record:        in.Record,
		LogFile:       in.LogFile,
		ObserveEgress: in.ObserveEgress,
		EgressAllow:   in.EgressAllow,
	}
	if router != nil {
		boot.ElicitHandler = router.route
	}
	client, ws, err := bootRun(ctx, in, boot, runID)
	if err != nil {
		return nil, err
	}
	return &Session{runID: runID, root: client, ws: ws, elicit: router}, nil
}

// bootInput carries everything bootAgent needs to build an agent's image and
// start it speaking MCP. Manifest is optional, used only for the image's
// provenance labels.
type bootInput struct {
	Vesselfile *vesselfile.Vesselfile
	Manifest   *bundle.Manifest
	SourceDir  string
	ImageRef   string
	RunID      string

	// Network and Env are set by the orchestrator when wiring a USES tree;
	// empty for a single-cage run, which stays on the default network.
	Network string
	Env     map[string]string

	// Budget is the operator's override in micro-USD; 0 falls back to the
	// agent's advisory BUDGET.
	Budget int64

	// Cap is the attached agent's resolved cap. Empty only when a caller
	// bypasses bootRun; startAttachedAgent then applies the runtime default.
	Cap config.Cap

	// MaxLive, HostMax, and IdleTTL are the cage policy the working set enforces.
	MaxLive int
	HostMax int
	IdleTTL time.Duration

	// MachineMemCap is machine.memory_gib in bytes, 0 when unset. A value
	// above the machine's real memory is ignored and flagged.
	MachineMemCap int64

	// OpEnv and OpSecrets are operator value pools, injected per declared name.
	OpEnv     map[string]string
	OpSecrets map[string]string

	NoCache bool
	Managed bool

	// InjectBridge stages this host's linux companion over the bundle's
	// mcp-bridge at image build. Set on bundle-extraction boots; never for a
	// source-directory introspection, whose bridge stageBridge already owns.
	InjectBridge bool

	// Interaction injects VESSEL_INTERACTION; empty injects nothing.
	Interaction string

	// OnEvent observes activations and evictions. Nil off the daemon path.
	OnEvent func(Event)

	Record bool

	LogFile func(runID string) io.WriteCloser

	// ObserveEgress runs the egress proxy in audit mode for this boot.
	ObserveEgress bool

	// EgressAllow is the operator's per-run egress override for the root agent.
	EgressAllow []string

	// ElicitHandler routes the root's mid-call questions; interactive boots only.
	ElicitHandler mcp.ElicitHandler

	Stdout  io.Writer
	Stderr  io.Writer
	Verbose bool
}

// teardown accumulates cleanup steps and runs them in reverse, joining every
// error so one failed step never strands the rest.
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

// bootSession is the provisioned runtime a boot builds and starts containers
// against: the platform provisioner and a BuildKit client.
type bootSession struct {
	provisioner Provisioner
	bk          *BuildKit
}

// bootAgent provisions the runtime, builds the agent's image, starts its
// container, and opens an MCP session to it. A boot that fails partway runs
// the teardown it accumulated, so a failed boot leaks nothing.
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

	// A lone agent still needs a per-run internal network when it has a MODEL
	// (LLM gateway) or allow: egress (egress proxy); the gateways are the run's
	// only doors out. Without either it stays on the default network.
	model := manifestModel(in.Manifest)
	// The operator's per-run --egress adds to what the Vesselfile declares.
	allowHosts := unionHosts(egressHosts(manifestEgress(in.Manifest)), in.EgressAllow)

	// Refuse the run before starting anything if the cage plus gateways does
	// not fit the machine, the same admission a USES tree gets. Empty Cap means
	// a caller bypassed bootRun (introspection); use the runtime default.
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

	// Count the cage and its gateways against the host cap. HostMax is 0 only
	// for introspection's transient boot, which skips host accounting.
	if in.HostMax > 0 {
		baseline := 1 // the agent's own cage
		if model != "" {
			baseline++
		}
		if len(allowHosts) > 0 {
			baseline++
		}
		if err := reserveBaseline(baseline, in.HostMax); err != nil {
			return nil, nil, err
		}
		td.push(releaseBaseline(baseline))
	}

	egressNet := in.RunID + "-egress"
	// A cage with no broker to reach gets no network at all (loopback only).
	// A stdio server needs none, and this is what makes deny-default actually
	// mean no internet: without it the container joins the default bridge and
	// can reach anywhere. A model or egress puts it on a private network below.
	if in.Network == "" {
		in.Network = "none"
	}
	if model != "" || len(allowHosts) > 0 || in.ObserveEgress {
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
			llmCfg.Record = in.Record
			if err := startLLMGateway(ctx, sess, in.RunID, []string{network}, egressNet, llmCfg, in, td); err != nil {
				return nil, nil, err
			}
			in.Env[env.LLMURL] = llmURL(in.RunID, token)
		}

		if len(allowHosts) > 0 || in.ObserveEgress {
			for k, v := range egressProxyEnv(in.RunID) {
				in.Env[k] = v
			}
		}
	}

	// Operator env and secrets, scoped to this agent's own declarations. Runs
	// for a tool collection too, which may declare secrets without reasoning.
	if in.Env == nil {
		in.Env = map[string]string{}
	}
	if in.Vesselfile != nil {
		if err := injectOperatorValues(in.Env, in.Vesselfile.Env, in.Vesselfile.Secrets, in.Vesselfile.Optional, in.OpEnv, in.OpSecrets); err != nil {
			return nil, nil, fmt.Errorf("agent %s: %w", rootAgentKey, err)
		}
	}

	client, err := startAttachedAgent(ctx, sess, in, td)
	if err != nil {
		return nil, nil, err
	}

	// The egress proxy keys its allow-list by the agent's address, known only
	// once the container is running, so it starts after the agent. Audit mode
	// runs it even with no allow list, to record what the server reaches.
	if len(allowHosts) > 0 || in.ObserveEgress {
		if err := startEgressProxy(ctx, sess, in.RunID, egressNet, map[string]egressAgent{in.RunID: {Network: in.Network, Hosts: allowHosts}}, in, td); err != nil {
			return nil, nil, err
		}
	}

	booted = true
	return client, &workingSet{sess: sess, td: td}, nil
}

// newBootSession brings up the provisioner and a BuildKit client, pushing
// their Close onto td. First run provisions the macOS Lima VM behind the
// setup UI; when already ready it shows nothing.
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

	// BuildKit's gRPC API works over the forwarded socket, sidestepping the
	// cross-host snapshot pain container lifecycle hits.
	bk, err := DialBuildKit(ctx, provisioner.BuildKitAddress())
	if err != nil {
		return nil, err
	}
	td.push(bk.Close)

	return &bootSession{provisioner: provisioner, bk: bk}, nil
}

// buildImage builds in.ImageRef unless already present. Refs are
// content-addressed, so an existing ref is provably the same source; noCache
// forces a rebuild regardless. A transient upstream failure (a registry 5xx
// or dropped connection resolving a base image) is retried once: those clear
// in seconds, and without the retry a first-run import fails on someone
// else's outage.
func buildImage(ctx context.Context, sess *bootSession, in BuildInput, noCache bool, stderr io.Writer) error {
	if !noCache && imageExists(ctx, sess.provisioner, in.ImageRef) {
		return nil
	}
	// Injection happens only when a build will: an existing image's ref
	// already covers the injected companion, so the skip above is safe.
	if in.InjectBridge {
		if err := injectBridgeBinary(in.SourceDir, stderr); err != nil {
			return err
		}
	}
	in.NoCache = noCache
	err := buildWithProgress(ctx, sess.bk, in, stderr)
	if err == nil || ctx.Err() != nil || !isTransientBuildError(err) {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "transient registry error (%v); retrying the build once\n", err)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	return buildWithProgress(ctx, sess.bk, in, stderr)
}

// isTransientBuildError matches upstream failures worth one retry. Deliberate
// non-matches: 404/not-found (a typo'd base image never resolves) and build
// step failures (a broken RUN reruns identically).
func isTransientBuildError(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"500 Internal Server Error",
		"502 Bad Gateway",
		"503 Service Unavailable",
		"toomanyrequests",
		"connection reset by peer",
		"TLS handshake timeout",
		"i/o timeout",
		"unexpected EOF",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// startAttachedAgent builds the agent's image and starts its container
// attached over stdio, opening an MCP session. Stdin EOF is the container's
// exit signal; sub-agents start detached and speak HTTP instead.
func startAttachedAgent(ctx context.Context, sess *bootSession, in bootInput, td *teardown) (*mcp.Client, error) {
	if err := buildImage(ctx, sess, BuildInput{
		Vesselfile:   in.Vesselfile,
		Manifest:     in.Manifest,
		SourceDir:    in.SourceDir,
		ImageRef:     in.ImageRef,
		InjectBridge: in.InjectBridge,
	}, in.NoCache, in.Stderr); err != nil {
		return nil, err
	}

	// Never start the root uncapped.
	cap := in.Cap
	if cap == (config.Cap{}) {
		cap = defaultAgentCap
	}

	// Only the attached root carries the interaction mode; sub-agents always
	// have their parent as a live caller.
	if in.Interaction != "" {
		if in.Env == nil {
			in.Env = map[string]string{}
		}
		in.Env[env.Interaction] = in.Interaction
	}

	// limactl shell into the Lima VM on macOS; nerdctl directly on Linux.
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
	// Tee the agent's stderr to the durable log from its first byte; the build
	// progress above went to in.Stderr alone and stays out of the log.
	cmd.Stderr = in.Stderr
	if in.LogFile != nil {
		lf := in.LogFile(in.RunID)
		cmd.Stderr = io.MultiWriter(in.Stderr, lf)
		td.push(func() error { return lf.Close() })
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting container subprocess: %w", err)
	}
	td.push(func() error {
		// Closing stdin signals the agent to exit; --rm removes the container
		// and Wait reaps the subprocess. A signal kill here is the teardown
		// doing its job, so only a non-zero exit code (an actual crash) is
		// worth surfacing.
		_ = stdinPipe.Close()
		if err := cmd.Wait(); err != nil && !killedBySignal(err) {
			return fmt.Errorf("container subprocess exited with error: %w", err)
		}
		return nil
	})

	client, err := mcp.Connect(ctx, stdoutPipe, stdinPipe, mcp.WithElicitation(in.ElicitHandler))
	if err != nil {
		// The live container will not get its stdin EOF through the normal
		// path; kill it so the teardown's Wait reaps it.
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("MCP connect: %w", err)
	}
	td.push(client.Close)

	return client, nil
}

// killedBySignal reports whether a process was stopped by a signal. ExitCode
// is -1 only for a signal kill after Wait, which during teardown means we
// stopped it on purpose.
func killedBySignal(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == -1
}

// deriveImageRef returns the local containerd ref for an agent: the bundle
// basename tagged with the source files hash, so an unchanged agent reuses its
// image and a changed one rebuilds. Never pushed; the no-latest rule for USES
// does not apply.
func deriveImageRef(bundlePath string, m *bundle.Manifest) string {
	return imageRefFor(bundlePath, m.FilesHash, manifestUsesBridge(m))
}

// shortDigest returns the first 12 hex chars of a sha256 digest.
func shortDigest(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// deriveRunID names the run's container: the agent's friendly name plus a
// content-hash suffix. Operators see this id in ps, stop, and trace tooling.
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

// uniqueSuffix is random rather than a clock so two runs in the same instant
// cannot collide; the time fallback only matters if the OS RNG is unavailable.
func uniqueSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

// sanitizeRef reduces s to characters safe in an OCI ref component or
// containerd container id.
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
	// A container and network name must start with [a-zA-Z0-9]; '.', '-' and
	// '_' are legal only mid-name. Trim any that lead so a source or dir named
	// "_thing" does not derive an invalid network name and fail the run.
	out := strings.TrimLeft(b.String(), "._-")
	if out == "" {
		return "agent"
	}
	return out
}
