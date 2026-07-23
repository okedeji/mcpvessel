package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/mcp"
	"github.com/okedeji/mcpvessel/internal/mcpgateway"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/registry"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
)

// containerStopTimeout bounds teardown of one container or network: rm -f
// plus the limactl shell round-trip. On expiry the resource is abandoned to
// the next stray-resource sweep rather than hanging shutdown.
const containerStopTimeout = 30 * time.Second

// bootRun picks the boot path: no USES takes the single-cage path, one or
// more takes the tree path behind the MCP gateway.
func bootRun(ctx context.Context, in RunInput, boot bootInput, runID string) (*mcp.Client, *workingSet, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	// Run flags overlay Defaults only; a per-agent config cap still wins as
	// the more specific choice.
	res := cfg.Resources
	res.Defaults = overlayCap(in.Resources, res.Defaults)
	ops := operatorInputs{env: in.Env, secrets: in.Secrets, rootName: in.Name, models: cfg.Models, resources: res, managed: in.Managed, prewarm: cfg.Cages.EffectivePrewarm(), keepWarm: cfg.Cages.KeepWarm, maxLive: cfg.Cages.EffectiveMaxLive(), record: in.Record, egressAllow: in.EgressAllow, configEgress: cfg.Egress}

	// The machine memory cap applies to both boot paths; the live-cage caps
	// and idle TTL only bound a USES tree's elastic set.
	boot.MachineMemCap = cfg.Machine.MemoryBytes()

	if len(boot.Manifest.Vesselfile.Uses) == 0 {
		// No registry ref, so per-agent overrides do not key a directly-run
		// agent; the default caps still apply. The operator's persisted egress
		// for this ref is unioned in, so a host approved on a past run is not
		// asked about again.
		boot.EgressAllow = append(boot.EgressAllow, configEgressForRef(in.Ref, cfg.Egress)...)
		boot.Cap = agentCap(nil, ops.resources)
		// A single cage counts against the host cap like a tree's do.
		boot.HostMax = cfg.Cages.EffectiveHostMaxLive()
		return bootAgent(ctx, boot)
	}

	tree, err := resolveRunTree(ctx, runID, in.BundlePath, boot.Manifest)
	if err != nil {
		return nil, nil, err
	}
	warnSecretShapes(in.Stderr, tree, in.Name, in.Secrets, configSecretScopes(cfg))
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

// warnSecretShapes flags two grant shapes worth an operator's eye before a
// tree boots. A broadcast-granted secret that any non-root agent declares
// reaches that agent, which is legitimate but also the exact shape of a
// sub-agent harvesting a credential meant for the root or a sibling; naming it
// lets the operator scope the grant. One malicious declarer is enough, so this
// fires whenever a broadcast secret reaches at least one non-root agent, not
// only when two or more declare it. A scope matching no agent in the run grants
// nothing, silently, which is almost always a typo, unless the scope came from
// a per-agent config binding (cfgScopes), which is global operator config and
// legitimately covers agents other runs use, so it is not warned about here.
func warnSecretShapes(w io.Writer, tree *runTree, rootName string, secrets ScopedSecrets, cfgScopes map[string]bool) {
	if w == nil || len(secrets) == 0 {
		return
	}
	scopes := map[string]bool{rootName: true}
	declaredBy := map[string][]string{}
	for _, key := range sortedNodeKeys(tree.Nodes) {
		node := tree.Nodes[key]
		scope := rootName
		if key != tree.Root {
			scope = usesAlias(node.Ref.Repository)
			scopes[scope] = true
		}
		if node.Manifest == nil {
			continue
		}
		for _, name := range node.Manifest.Vesselfile.Secrets {
			declaredBy[name] = append(declaredBy[name], scope)
		}
	}
	names := make([]string, 0, len(declaredBy))
	for name := range declaredBy {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, broadcast := secrets[""][name]; !broadcast {
			continue
		}
		// Fire if any non-root agent receives this broadcast secret: a single
		// sub-agent declaring the name is enough to harvest it, so the
		// two-or-more-declarers shape is not the only risk worth surfacing.
		nonRoot := false
		for _, scope := range declaredBy[name] {
			if scope != rootName {
				nonRoot = true
				break
			}
		}
		if !nonRoot {
			continue
		}
		_, _ = fmt.Fprintf(w, "warning: agents %s declare secret %s and the broadcast grant reaches every one of them; scope it with --secret <agent>:%s to grant only the agent that should have it\n",
			strings.Join(declaredBy[name], ", "), name, name)
	}
	for _, scope := range secrets.Scopes() {
		if !scopes[scope] && !cfgScopes[scope] {
			_, _ = fmt.Fprintf(w, "warning: --secret scope %q matches no agent in this run; nothing was granted under it\n", scope)
		}
	}
}

// configSecretScopes maps each per-agent config secret binding to the USES
// alias it scopes to in a pool, mirroring how applyConfigSecrets injects it.
func configSecretScopes(cfg *config.Config) map[string]bool {
	scopes := map[string]bool{}
	for key := range cfg.Secrets.Agents {
		r, err := reference.Parse(key)
		if err != nil || r.Repository == "" {
			continue
		}
		scopes[usesAlias(r.Repository)] = true
	}
	return scopes
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

// bootTree boots a USES tree: networks, prewarmed sub-agents, the MCP
// gateway, then the root attached over stdio. The root boots last so
// everything it calls is already listening; teardown reverses the order.
func bootTree(ctx context.Context, in bootInput, tree *runTree, plan *runPlan, runID string) (*mcp.Client, *workingSet, error) {
	td := &teardown{}
	booted := false
	// Prewarmed cages live in the working set, not on td (a reaped cage must
	// not also sit on the teardown stack), so a failed boot removes them here.
	// sess is nil until newBootSession returns, but started is empty until
	// after that, so the loop never derefs nil.
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

	// Before any container starts: refuse a run whose always-on baseline
	// cannot fit the machine, and clamp the elastic cap to the leftover
	// memory so on-demand growth cannot OOM the host.
	usable, err := usableMemory(sess.provisioner, in.MachineMemCap, in.Stderr)
	if err != nil {
		return nil, nil, err
	}
	maxLive, err := fitElastic(usable, plan, in.MaxLive)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if maxLive < in.MaxLive {
		note := fmt.Sprintf("cages.max_live reduced from %d to %d to fit available memory", in.MaxLive, maxLive)
		warnings = append(warnings, note)
		_, _ = fmt.Fprintf(in.Stderr, "note: %s\n", note)
	}

	// Reserve host slots for the always-on baseline (root + gateway
	// singletons) up front: host_max_live counts the whole run, not just the
	// elastic sub-agents. Prewarmed sub-agents reserve their own slots in the
	// skeleton loop.
	baseline := 2 // root + the MCP gateway, present in every tree
	if len(plan.LLMAgents) > 0 {
		baseline++
	}
	if len(plan.EgressAgents) > 0 {
		baseline++
	}
	if err := reserveBaseline(baseline, in.HostMax); err != nil {
		return nil, nil, err
	}
	td.push(releaseBaseline(baseline))

	// All internal networks are created up front, before the MCP gateway
	// joins them. Each cage is alone on its network, so no cage can reach a
	// sibling directly and bypass the gateway's deny. Each remove is pushed
	// before the containers that join it, so teardown removes containers first.
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

	// The skeleton boots only prewarmed agents; the rest activate on first
	// call. Each skeleton cage reserves a host slot: a skeleton that does not
	// fit fails the boot rather than overcommitting the host. The per-run cap
	// does not apply here; it bounds elastic growth on top of the skeleton.
	reasonFree := append([]string{}, plan.ReasonPool...)
	plainFree := append([]string{}, plan.PlainPool...)
	netOf := map[string]string{}
	state := map[string]cageState{}
	lastUse := map[string]time.Time{}
	addr := map[string]string{}
	now := nowFunc()

	// startCage builds and starts one cage and resolves its address. The
	// gateway routes by IP, not name: its /etc/hosts is frozen at its own
	// start and cannot name a cage that activates later. Both the skeleton
	// loop and on-demand activation go through here.
	startCage := func(ctx context.Context, pa plannedAgent) (string, error) {
		if err := buildAgentImage(ctx, sess, pa.Node, pa.Spec.ImageRef, in.NoCache, in.Stderr); err != nil {
			return "", fmt.Errorf("activating %s: %w", pa.Node.Key, err)
		}
		if err := startDetached(ctx, sess.provisioner, pa.Spec); err != nil {
			return "", fmt.Errorf("activating %s: %w", pa.Node.Key, err)
		}
		ip, err := containerIP(ctx, sess.provisioner, pa.Spec.RunID)
		if err != nil {
			return "", fmt.Errorf("activating %s: %w", pa.Node.Key, err)
		}
		return agentTarget(ip), nil
	}

	for _, a := range plan.Agents {
		if !a.Prewarm {
			continue
		}
		if !hostCages.tryReserve(in.HostMax) {
			return nil, nil, fmt.Errorf("host at capacity: the run's skeleton does not fit in cages.host_max_live (%d); raise it or lower cages.prewarm", in.HostMax)
		}
		hostReserved++
		pa := a
		if !a.AlwaysWarm {
			net, err := popPoolNet(&reasonFree, &plainFree, plan.LLMAgents[a.Node.Key] != "")
			if err != nil {
				return nil, nil, err
			}
			pa.Spec.Networks = []string{net}
			netOf[a.Node.Key] = net
		}
		cageAddr, err := startCage(ctx, pa)
		if err != nil {
			return nil, nil, err
		}
		started = append(started, pa.Spec.RunID)
		state[a.Node.Key] = cageLive
		lastUse[a.Node.Key] = now
		addr[a.Node.Key] = cageAddr
	}

	// The MCP gateway is the only host the parent's USES URLs resolve to, so
	// it sees every call in the tree and enforces every edge's deny. Its
	// image is keyed by version: an existing one is current, skip the rebuild.
	if in.NoCache || !imageExists(ctx, sess.provisioner, plan.MCPGateway.ImageRef) {
		if err := BuildGatewayImage(ctx, sess.bk, in.NoCache, in.Stderr); err != nil {
			return nil, nil, err
		}
	}
	if err := startDetached(ctx, sess.provisioner, plan.MCPGateway); err != nil {
		return nil, nil, err
	}
	td.push(func() error { return removeContainer(sess.provisioner, plan.MCPGateway.RunID) })

	// One non-internal network is the sole door outside, shared by the LLM
	// gateway and egress proxy. It exists only when the run reasons or an
	// agent declares allow:; otherwise the tree stays fully internal.
	var egressNet string
	if len(plan.LLMAgents) > 0 || len(plan.EgressAgents) > 0 {
		egressNet = runID + "-egress"
		if err := createNetwork(ctx, sess.provisioner, egressNet, false, in.Managed); err != nil {
			return nil, nil, err
		}
		td.push(func() error { return removeNetwork(sess.provisioner, egressNet) })
	}

	// Boots before the root so a reasoning root finds VESSEL_LLM_URL
	// already listening.
	if len(plan.LLMAgents) > 0 {
		budget := resolveBudget(in.Budget, plan.Budget, in.Stderr)
		llmCfg, err := buildLLMConfig(plan.LLMAgents, plan.LLMTokens, budget)
		if err != nil {
			return nil, nil, err
		}
		llmCfg.Record = in.Record
		if err := startLLMGateway(ctx, sess, runID, plan.LLMNets, egressNet, llmCfg, in, td); err != nil {
			return nil, nil, err
		}
	}

	in.Network = plan.RootNet
	in.Env = plan.RootEnv
	client, err := startAttachedAgent(ctx, sess, in, plan.RootSecretEnv, td)
	if err != nil {
		return nil, nil, err
	}

	// The egress proxy starts after the root: only then does every allow:
	// agent, root included, have an address to key its allow-list by.
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
	edgesByNode := map[string][]string{}
	for edge, node := range plan.EdgeNodes {
		edgesByNode[node] = append(edgesByNode[node], edge)
	}
	ws := &workingSet{
		sess:           sess,
		plan:           plan,
		tree:           tree,
		td:             td,
		specByNode:     specByNode,
		alwaysWarm:     alwaysWarm,
		reasonFree:     reasonFree,
		plainFree:      plainFree,
		netOf:          netOf,
		state:          state,
		pins:           map[string]int{},
		lastUse:        lastUse,
		inflight:       map[string]*activation{},
		addr:           addr,
		edgesByNode:    edgesByNode,
		maxLive:        maxLive,
		hostMax:        in.HostMax,
		idleTTL:        in.IdleTTL,
		slotFreed:      make(chan struct{}, 1),
		saturationWait: saturationWaitDefault,
		outbound:       make(chan mcpgateway.ControlMessage, 256),
		elicit:         in.ElicitHandler,
		startCage:      startCage,
		stopCage:       func(name string) error { return removeContainer(sess.provisioner, name) },
		stderr:         in.Stderr,
		warnings:       warnings,
		onEvent:        in.OnEvent,
		runID:          in.RunID,
	}
	return client, ws, nil
}

// buildAgentImage extracts a sub-agent's bundle to a temp dir, reparses its
// Vesselfile, and builds its image. The extracted source is deleted once the
// image lands in containerd.
func buildAgentImage(ctx context.Context, sess *bootSession, node *agentNode, imageRef string, noCache bool, stderr io.Writer) error {
	// Check before paying to extract: a present content-addressed image
	// needs no source at all.
	if !noCache && imageExists(ctx, sess.provisioner, imageRef) {
		return nil
	}

	srcDir, err := os.MkdirTemp("", "mcpvessel-sub-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	manifest, err := bundle.Extract(node.Bundle, srcDir)
	if err != nil {
		return fmt.Errorf("extracting %s: %w", node.Key, err)
	}
	af, err := vesselfile.ParseFile(filepath.Join(srcDir, "Vesselfile"))
	if err != nil {
		return fmt.Errorf("re-parsing %s Vesselfile: %w", node.Key, err)
	}
	return buildImage(ctx, sess, BuildInput{
		Vesselfile:   af,
		Manifest:     manifest,
		SourceDir:    srcDir,
		ImageRef:     imageRef,
		InjectBridge: manifestUsesBridge(manifest),
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
	cmd := p.Nerdctl(ctx, nerdctlRunArgs(spec)...)
	// When the spec carries secret values, nerdctlRunArgs added
	// `--env-file /dev/stdin`; feed the KEY=VALUE lines over stdin so the values
	// never appear on argv. A detached container reads no stdin of its own, so
	// this is safe. runNerdctl sets stdout/stderr, so only stdin is set here.
	if content := secretEnvFile(spec); content != "" {
		cmd.Stdin = strings.NewReader(content)
	}
	return runNerdctl(cmd, "starting "+spec.RunID)
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
