package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/mcp"
	"github.com/okedeji/agentcage/internal/mcpgateway"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
)

// containerStopTimeout bounds how long teardown waits for one detached
// container or network to go away. rm -f kills and removes; 30s leaves room
// for that plus the limactl shell round-trip into the VM. Exceeding it
// abandons the container to the next stray-resource sweep rather than
// hanging the operator's shutdown.
const containerStopTimeout = 30 * time.Second

// bootRun picks the boot path by whether the agent declares any USES: no
// dependencies takes today's single-container path unchanged; one or more
// takes the tree path that starts every sub-agent behind the MCP gateway.
func bootRun(ctx context.Context, in RunInput, boot bootInput, runID string) (*mcp.Client, *workingSet, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	// The run's --memory/--cpus/--pids flags override the configured default
	// cap per field; a per-agent config cap still wins as the more specific
	// choice, so this overlays onto Defaults only.
	res := cfg.Resources
	res.Defaults = overlayCap(in.Resources, res.Defaults)
	ops := operatorInputs{env: in.Env, secrets: in.Secrets, models: cfg.Models, resources: res, managed: in.Managed, prewarm: cfg.Cages.EffectivePrewarm(), alwaysWarm: cfg.Cages.AlwaysWarm, maxLive: cfg.Cages.EffectiveMaxLive()}

	if len(boot.Manifest.Agentfile.Uses) == 0 {
		// A directly-run agent has no registry ref, so per-agent overrides do
		// not key it; the operator default cap and the runtime default still do.
		boot.Cap = agentCap(nil, ops.resources)
		return bootAgent(ctx, boot)
	}

	tree, err := resolveRunTree(ctx, runID, in.BundlePath, boot.Manifest)
	if err != nil {
		return nil, nil, err
	}
	plan, err := buildRunPlan(tree, runID, ops)
	if err != nil {
		return nil, nil, err
	}
	boot.Cap = plan.RootCap
	boot.MaxLive = cfg.Cages.EffectiveMaxLive()
	boot.HostMax = cfg.Cages.EffectiveHostMaxLive()
	boot.IdleTTL = cfg.Cages.EffectiveIdleTTL()
	return bootTree(ctx, boot, tree, plan, runID)
}

// resolveRunTree walks the root's transitive USES graph, pulling each
// dependency from the registry by its locked digest.
func resolveRunTree(ctx context.Context, runID, rootBundle string, root *bundle.Manifest) (*runTree, error) {
	reg, err := registry.New()
	if err != nil {
		return nil, fmt.Errorf("registry client: %w", err)
	}
	pull := func(ctx context.Context, ref reference.Reference) (string, *bundle.Manifest, error) {
		path, _, err := reg.Pull(ctx, ref)
		if err != nil {
			return "", nil, err
		}
		m, err := bundle.ReadManifest(path)
		if err != nil {
			return "", nil, fmt.Errorf("reading pulled manifest %s: %w", ref.OCIRef(), err)
		}
		return path, m, nil
	}
	return resolveTree(ctx, runID, rootBundle, root, pull)
}

// bootTree starts a parent whose bundle has USES dependencies: a per-run
// network, every sub-agent detached and serving HTTP on it, the MCP gateway
// carrying the routing table, and finally the root parent attached over
// stdio with its sub-agent URLs. The order matters: the network exists
// before anything joins it, and the root boots last so the MCP gateway and
// sub-agents it calls are already listening. Teardown reverses all of it.
func bootTree(ctx context.Context, in bootInput, tree *runTree, plan *runPlan, runID string) (*mcp.Client, *workingSet, error) {
	td := &teardown{}
	booted := false
	// Prewarmed cages are tracked in the working set, not on td (so a reaped one
	// is not also queued for release), so a boot that fails partway removes them
	// here rather than through the teardown stack. sess is nil until newBootSession
	// returns, but started is empty until after that, so the loop never derefs it.
	var sess *bootSession
	var started []string
	hostReserved := 0
	defer func() {
		if !booted {
			for _, name := range started {
				_ = removeContainer(sess.provisioner, name)
			}
			for i := 0; i < hostReserved; i++ {
				hostCages.release()
			}
			_ = td.run()
		}
	}()

	sess, err := newBootSession(ctx, in, td)
	if err != nil {
		return nil, nil, err
	}

	// Every network is internal and created up front, before the MCP gateway joins
	// all of them: the dedicated networks for the root and always-warm cages, and
	// the two pools pooled cages draw from. Each cage is alone on its network, so
	// no cage can reach a sibling directly and bypass the MCP gateway's deny. Each
	// remove is pushed before the containers that join it, so teardown (reverse
	// order) removes the containers first.
	allNets := make([]string, 0, len(plan.AgentNets)+len(plan.ReasonPool)+len(plan.PlainPool))
	for _, key := range sortedStringKeys(plan.AgentNets) {
		allNets = append(allNets, plan.AgentNets[key])
	}
	allNets = append(allNets, plan.ReasonPool...)
	allNets = append(allNets, plan.PlainPool...)
	for _, net := range allNets {
		net := net
		if err := createNetwork(ctx, sess.provisioner, net, true, in.Managed); err != nil {
			return nil, nil, err
		}
		td.push(func() error { return removeNetwork(sess.provisioner, net) })
	}

	// The skeleton boots only the prewarmed agents (the root's direct children
	// plus the pinned-warm ones); the rest activate on first call. A pooled
	// prewarmed cage draws a network from its pool here; an always-warm one already
	// carries its dedicated network. Every skeleton cage reserves a slot against
	// the host capacity: the compulsory always-warm baseline is guaranteed only up
	// to what the machine can hold, so a skeleton that does not fit fails the boot
	// with a clear error rather than overcommitting the host. The per-run cap does
	// not apply here; it bounds the elastic growth on top of the skeleton.
	reasonFree := append([]string{}, plan.ReasonPool...)
	plainFree := append([]string{}, plan.PlainPool...)
	netOf := map[string]string{}
	state := map[string]cageState{}
	lastUse := map[string]time.Time{}
	now := nowFunc()
	for _, a := range plan.Agents {
		if !a.Prewarm {
			continue
		}
		if !hostCages.tryReserve(in.HostMax) {
			return nil, nil, fmt.Errorf("host at capacity: the run's skeleton does not fit in cages.host_max_live (%d); raise it or lower cages.prewarm", in.HostMax)
		}
		hostReserved++
		spec := a.Spec
		if !a.AlwaysWarm {
			net, err := popPoolNet(&reasonFree, &plainFree, plan.LLMAgents[a.Node.Key] != "")
			if err != nil {
				return nil, nil, err
			}
			spec.Networks = []string{net}
			netOf[a.Node.Key] = net
		}
		if err := buildAgentImage(ctx, sess, a.Node, a.Spec.ImageRef, in.NoCache, in.Stderr); err != nil {
			return nil, nil, err
		}
		if err := startDetached(ctx, sess.provisioner, spec); err != nil {
			return nil, nil, err
		}
		started = append(started, spec.RunID)
		state[a.Node.Key] = cageLive
		lastUse[a.Node.Key] = now
	}

	// The MCP gateway is the only host the parent's USES URLs resolve to, so it
	// sees every call in the tree and enforces every edge's deny. Its image
	// is keyed by version, so an existing one is current; skip rebuilding it.
	if in.NoCache || !imageExists(ctx, sess.provisioner, plan.MCPGateway.ImageRef) {
		if err := BuildGatewayImage(ctx, sess.bk, in.NoCache, in.Stderr); err != nil {
			return nil, nil, err
		}
	}
	if err := startDetached(ctx, sess.provisioner, plan.MCPGateway); err != nil {
		return nil, nil, err
	}
	td.push(func() error { return removeContainer(sess.provisioner, plan.MCPGateway.RunID) })

	// One non-internal network is the door to the outside for both the LLM
	// gateway and the egress proxy. It exists whenever the run reasons or any
	// agent declares allow:, and only then, so a tree that needs neither stays
	// fully internal.
	var egressNet string
	if len(plan.LLMAgents) > 0 || len(plan.EgressAgents) > 0 {
		egressNet = runID + "-egress"
		if err := createNetwork(ctx, sess.provisioner, egressNet, false, in.Managed); err != nil {
			return nil, nil, err
		}
		td.push(func() error { return removeNetwork(sess.provisioner, egressNet) })
	}

	// The LLM gateway boots after the MCP gateway and before the root, so a
	// reasoning root finds its AGENTCAGE_LLM_URL already listening. It is
	// skipped entirely when nothing in the tree reasons.
	if len(plan.LLMAgents) > 0 {
		budget := resolveBudget(in.Budget, plan.Budget, in.Stderr)
		llmCfg, err := buildLLMConfig(plan.LLMAgents, plan.LLMTokens, budget)
		if err != nil {
			return nil, nil, err
		}
		if err := startLLMGateway(ctx, sess, runID, plan.LLMNets, egressNet, llmCfg, in, td); err != nil {
			return nil, nil, err
		}
	}

	in.Network = plan.RootNet
	in.Env = plan.RootEnv
	client, err := startAttachedAgent(ctx, sess, in, td)
	if err != nil {
		return nil, nil, err
	}

	// The egress proxy starts after the root because the root runs attached and
	// last, so this is the first point every allow: agent in the tree, the root
	// included, has an address to key its allow-list by.
	if len(plan.EgressAgents) > 0 {
		if err := startEgressProxy(ctx, sess, runID, egressNet, plan.EgressAgents, in, td); err != nil {
			return nil, nil, err
		}
	}

	booted = true
	specByNode := make(map[string]plannedAgent, len(plan.Agents))
	alwaysWarm := map[string]bool{}
	for _, a := range plan.Agents {
		specByNode[a.Node.Key] = a
		if a.AlwaysWarm {
			alwaysWarm[a.Node.Key] = true
		}
	}
	ws := &workingSet{
		sess:       sess,
		plan:       plan,
		tree:       tree,
		td:         td,
		specByNode: specByNode,
		alwaysWarm: alwaysWarm,
		reasonFree: reasonFree,
		plainFree:  plainFree,
		netOf:      netOf,
		state:      state,
		pins:       map[string]int{},
		lastUse:    lastUse,
		inflight:   map[string]*activation{},
		maxLive:    in.MaxLive,
		hostMax:    in.HostMax,
		idleTTL:    in.IdleTTL,
		outbound:   make(chan mcpgateway.ControlMessage, 256),
		noCache:    in.NoCache,
		stderr:     in.Stderr,
	}
	return client, ws, nil
}

// buildAgentImage extracts a sub-agent's bundle to a temp dir, reparses its
// Agentfile, and builds its image. The source is only needed during the
// build, so it is removed as soon as the image lands in containerd.
func buildAgentImage(ctx context.Context, sess *bootSession, node *agentNode, imageRef string, noCache bool, stderr io.Writer) error {
	// A present content-addressed image needs no source at all, so check
	// before paying to extract and reparse the bundle.
	if !noCache && imageExists(ctx, sess.provisioner, imageRef) {
		return nil
	}

	srcDir, err := os.MkdirTemp("", "agentcage-sub-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	manifest, err := bundle.Extract(node.Bundle, srcDir)
	if err != nil {
		return fmt.Errorf("extracting %s: %w", node.Key, err)
	}
	af, err := agentfile.ParseFile(filepath.Join(srcDir, "Agentfile"))
	if err != nil {
		return fmt.Errorf("re-parsing %s Agentfile: %w", node.Key, err)
	}
	return buildImage(ctx, sess, BuildInput{
		Agentfile: af,
		Manifest:  manifest,
		SourceDir: srcDir,
		ImageRef:  imageRef,
	}, noCache, stderr)
}

// createNetwork creates a per-run nerdctl network. An internal network has no
// route off the host, so a cage on it cannot egress: that is what makes
// EGRESS deny-default hold. The gateway doors that need the outside join a
// second, non-internal network too.
func createNetwork(ctx context.Context, p Provisioner, name string, internal, managed bool) error {
	return runNerdctl(p.Nerdctl(ctx, networkCreateArgs(name, internal, managed)...), "creating network "+name)
}

func networkCreateArgs(name string, internal, managed bool) []string {
	args := []string{"network", "create", name}
	if internal {
		args = append(args, "--internal")
	}
	if managed {
		args = append(args, "--label", daemonResourceLabel+"=1")
	}
	return args
}

// imageExists reports whether a local image with exactly ref is present.
// nerdctl image inspect is unreliable here (its name resolution differs from
// the one run uses), so we list every image by repo:tag and match exactly.
func imageExists(ctx context.Context, p Provisioner, ref string) bool {
	cmd := p.Nerdctl(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return false
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == ref {
			return true
		}
	}
	return false
}

func startDetached(ctx context.Context, p Provisioner, spec ContainerSpec) error {
	return runNerdctl(p.Nerdctl(ctx, nerdctlRunArgs(spec)...), "starting "+spec.RunID)
}

// removeNetwork and removeContainer run at teardown on a fresh context: the
// boot context may already be cancelled (operator Ctrl-C) and the resources
// still have to come down. The deadline keeps a wedged removal from hanging
// shutdown.
func removeNetwork(p Provisioner, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerStopTimeout)
	defer cancel()
	return runNerdctl(p.Nerdctl(ctx, "network", "rm", name), "removing network "+name)
}

func removeContainer(p Provisioner, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerStopTimeout)
	defer cancel()
	// rm -f stops and removes in one step. Detached containers carry no
	// --rm, so this is what reaps them; -f also makes it idempotent against
	// a container that already exited.
	return runNerdctl(p.Nerdctl(ctx, "rm", "-f", name), "removing "+name)
}

// runNerdctl runs a nerdctl command, discarding stdout and folding captured
// stderr into the error so a failed network or container op says why.
func runNerdctl(cmd *exec.Cmd, action string) error {
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", action, err, msg)
		}
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}
