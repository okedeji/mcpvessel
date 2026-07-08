package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/mcpgateway"
	"github.com/okedeji/agentcage/internal/reference"
)

const (
	// agentServePort is the streamable-HTTP port every sub-agent serves on.
	// Each agent is its own cage, so the port never collides.
	agentServePort = "8000"

	// mcpServePath is where a sub-agent serves MCP: the agent binds it, the
	// MCP gateway targets it.
	mcpServePath = "/mcp"
)

// agentTarget is the gateway's forward URL for a sub-agent reachable at host.
// One owner of the URL shape so both ends always agree.
func agentTarget(host string) string {
	return "http://" + host + ":" + agentServePort + mcpServePath
}

// runPlan is everything the orchestrator needs to start a USES tree. Derived
// purely from the resolved tree, so the security-load-bearing wiring (which
// agent sits on which network, which edge denies what) is unit tested without
// starting a container.
type runPlan struct {
	MCPGatewayCfg mcpgateway.Config
	MCPGateway    ContainerSpec
	Agents        []plannedAgent
	RootEnv       map[string]string

	// AgentNets maps agents with a dedicated network: the root and the
	// always-warm cages. Pooled cages draw a network at activation instead.
	// Each cage alone on its own network is what stops it reaching a sibling
	// and bypassing the MCP gateway's deny. RootNet is carried out because the
	// root starts outside the sub-agent loop.
	AgentNets map[string]string
	RootNet   string

	// ReasonPool and PlainPool are the reusable networks pooled cages draw
	// from. The LLM gateway joins only the reasoning pool, so a non-reasoning
	// cage never shares a network with the key holder. Each is sized to the
	// per-run live cap; one cage occupies a pool network at a time, so sibling
	// isolation holds across reuse.
	ReasonPool []string
	PlainPool  []string

	// LLMNets are the networks the LLM gateway joins: the dedicated networks
	// of reasoning cages plus the whole reasoning pool.
	LLMNets []string

	// LLMAgents maps each reasoning agent's key to its advisory model; empty
	// means no LLM gateway is needed. LLMTokens holds the unguessable token
	// each agent's AGENTCAGE_LLM_URL carries; the gateway routes by token, not
	// the guessable agent key. Budget is the root's advisory cap in micro-USD.
	LLMAgents map[string]string
	LLMTokens map[string]string
	Budget    int64

	// EgressAgents maps each allow: agent's container name to its network and
	// permitted hosts; empty means no egress proxy is needed.
	EgressAgents map[string]egressAgent

	// EdgeNodes folds a gateway edge's capability token back to the sub-agent
	// node it routes to, which is how an activate request (naming only an
	// edge) finds the node to boot.
	EdgeNodes map[string]string

	// RootCap is the attached root's resolved cap, resolved here because the
	// root runs outside the sub-agent loop.
	RootCap config.Cap
}

// egressAgent is one allow: agent's egress wiring: the network the proxy joins
// to reach it, and the hosts it may reach.
type egressAgent struct {
	Network string
	Hosts   []string
}

// plannedAgent pairs a non-root tree node with its detached container spec.
// Prewarm boots with the skeleton; the rest activate on first call. AlwaysWarm
// is never reaped or evicted.
type plannedAgent struct {
	Node       *agentNode
	Spec       ContainerSpec
	Prewarm    bool
	AlwaysWarm bool
}

// buildRunPlan turns a resolved tree into the containers, networks, and MCP
// gateway routing for a run. runID scopes every name so concurrent runs do not
// collide. Each USES edge becomes one gateway route (target plus deny) and one
// injected AGENTCAGE_USES_<ALIAS>_URL on the caller, so every call in the tree
// passes the referee that enforces deny.
func buildRunPlan(tree *runTree, runID string, ops operatorInputs) (*runPlan, error) {
	mcpGatewayName := runID + "-gw"

	containerName := func(key string) string {
		if key == tree.Root {
			return runID
		}
		return runID + "-" + key
	}
	nodeNet := func(key string) string {
		return runID + "-" + sanitizeRef(key) + "-net"
	}

	plan := &runPlan{
		MCPGatewayCfg: mcpgateway.Config{Edges: map[string]mcpgateway.Edge{}},
		RootEnv:       map[string]string{},
		AgentNets:     map[string]string{},
		LLMAgents:     map[string]string{},
		LLMTokens:     map[string]string{},
		EgressAgents:  map[string]egressAgent{},
		EdgeNodes:     map[string]string{},
		Budget:        nodeBudget(tree.Nodes[tree.Root]),
	}

	wholeBanned, toolBanned, err := classifyBans(tree)
	if err != nil {
		return nil, err
	}

	// pinnedWarm: nodes kept warm for the run's life. EGRESS allow: agents
	// cannot be lazy or reaped (the egress proxy keys them by an IP that must
	// exist at boot and stay put); operator keep_warm entries join them.
	keepWarm := map[string]bool{}
	for _, ref := range ops.keepWarm {
		keepWarm[ref] = true
	}
	pinnedWarm := map[string]bool{}
	for key, node := range tree.Nodes {
		if key == tree.Root || wholeBanned[key] {
			continue
		}
		if len(egressHosts(nodeEgress(node))) > 0 || keepWarm[refKey(node)] {
			pinnedWarm[key] = true
		}
	}

	// Pooled cages (not pinned-warm, root, or banned) draw from one of two
	// pools sized to the live cap, capped by how many cages of that kind the
	// tree has, keeping network count bounded well under the CNI wall.
	// Reasoning cages (a MODEL) use the pool the LLM gateway joins; the rest
	// the plain pool.
	poolNet := func(kind string, i int) string {
		return runID + "-" + kind + "pool-" + strconv.Itoa(i)
	}
	var reasonCount, plainCount int
	for key, node := range tree.Nodes {
		if key == tree.Root || wholeBanned[key] || pinnedWarm[key] {
			continue
		}
		if nodeModel(node) != "" {
			reasonCount++
		} else {
			plainCount++
		}
	}
	for i := 0; i < min(ops.maxLive, reasonCount); i++ {
		plan.ReasonPool = append(plan.ReasonPool, poolNet("r", i))
	}
	for i := 0; i < min(ops.maxLive, plainCount); i++ {
		plan.PlainPool = append(plan.PlainPool, poolNet("p", i))
	}

	// Prewarm the root's direct children (the hot path, up to the operator's
	// count, in sorted order for determinism) plus every pinned-warm node. The
	// rest get inactive edges: the gateway holds their first call while the
	// daemon boots the sub-agent.
	prewarmed := map[string]bool{}
	for key := range pinnedWarm {
		prewarmed[key] = true
	}
	seenChild := map[string]bool{}
	var directChildren []string
	for _, e := range tree.Edges {
		if e.Caller == tree.Root && !seenChild[e.Sub] {
			seenChild[e.Sub] = true
			directChildren = append(directChildren, e.Sub)
		}
	}
	sort.Strings(directChildren)
	for i, sub := range directChildren {
		if i >= ops.prewarm {
			break
		}
		prewarmed[sub] = true
	}

	// callerEnv collects the sub-agent URLs each non-root caller is injected
	// with; the root's go straight into plan.RootEnv.
	callerEnv := map[string]map[string]string{}
	for _, e := range tree.Edges {
		edgeKey, err := capabilityToken()
		if err != nil {
			return nil, err
		}
		edge := mcpgateway.Edge{
			Target: agentTarget(containerName(e.Sub)),
		}
		if wholeBanned[e.Sub] {
			edge.Banned = true
		} else {
			edge.Deny = mergeDeny(e.Deny, toolBanned[e.Sub])
			edge.Inactive = !prewarmed[e.Sub]
		}
		plan.MCPGatewayCfg.Edges[edgeKey] = edge
		plan.EdgeNodes[edgeKey] = e.Sub

		// A banned edge still gets its URL injected: the caller reaches the
		// gateway and gets a clean banned error rather than a missing variable.
		url := "http://" + mcpGatewayName + ":" + env.DefaultMCPGatewayPort + "/" + edgeKey + mcpServePath
		if e.Caller == tree.Root {
			plan.RootEnv[env.UsesURL(e.Alias)] = url
			continue
		}
		if callerEnv[e.Caller] == nil {
			callerEnv[e.Caller] = map[string]string{}
		}
		callerEnv[e.Caller][env.UsesURL(e.Alias)] = url
	}

	for _, key := range sortedNodeKeys(tree.Nodes) {
		if key == tree.Root || wholeBanned[key] {
			// A banned agent never starts; its edges are rejected at the MCP gateway.
			continue
		}
		agentEnv := map[string]string{env.ServeHTTP: ":" + agentServePort}
		for k, v := range callerEnv[key] {
			agentEnv[k] = v
		}
		node := tree.Nodes[key]
		// Pinned-warm cages hold a dedicated network; pooled cages draw one
		// at activation.
		var nets []string
		if pinnedWarm[key] {
			plan.AgentNets[key] = nodeNet(key)
			nets = []string{nodeNet(key)}
		}
		if model := nodeModel(node); model != "" {
			token, err := capabilityToken()
			if err != nil {
				return nil, err
			}
			plan.LLMTokens[key] = token
			agentEnv[env.LLMURL] = llmURL(runID, token)
			plan.LLMAgents[key] = effectiveModel(model, node, ops.models)
		}
		if hosts := egressHosts(nodeEgress(node)); len(hosts) > 0 {
			plan.EgressAgents[containerName(key)] = egressAgent{Network: nodeNet(key), Hosts: hosts}
			for k, v := range egressProxyEnv(runID) {
				agentEnv[k] = v
			}
		}
		if err := injectOperatorValues(agentEnv, node.Manifest, ops.env, ops.secrets); err != nil {
			return nil, fmt.Errorf("agent %s: %w", key, err)
		}
		plan.Agents = append(plan.Agents, plannedAgent{
			Node: node,
			Spec: ContainerSpec{
				RunID:    containerName(key),
				ImageRef: agentImageRef(node),
				Networks: nets,
				Env:      agentEnv,
				Detached: true,
				Managed:  ops.managed,
			}.withCap(agentCap(node, ops.resources)),
			Prewarm:    prewarmed[key],
			AlwaysWarm: pinnedWarm[key],
		})
	}

	// The root runs attached, outside the loop above; wire it here.
	plan.AgentNets[tree.Root] = nodeNet(tree.Root)
	plan.RootNet = nodeNet(tree.Root)
	if model := nodeModel(tree.Nodes[tree.Root]); model != "" {
		token, err := capabilityToken()
		if err != nil {
			return nil, err
		}
		plan.LLMTokens[tree.Root] = token
		plan.RootEnv[env.LLMURL] = llmURL(runID, token)
		plan.LLMAgents[tree.Root] = effectiveModel(model, tree.Nodes[tree.Root], ops.models)
	}
	plan.RootCap = agentCap(tree.Nodes[tree.Root], ops.resources)
	if hosts := egressHosts(nodeEgress(tree.Nodes[tree.Root])); len(hosts) > 0 {
		plan.EgressAgents[containerName(tree.Root)] = egressAgent{Network: nodeNet(tree.Root), Hosts: hosts}
		for k, v := range egressProxyEnv(runID) {
			plan.RootEnv[k] = v
		}
	}
	if err := injectOperatorValues(plan.RootEnv, tree.Nodes[tree.Root].Manifest, ops.env, ops.secrets); err != nil {
		return nil, fmt.Errorf("agent %s: %w", tree.Root, err)
	}

	// The LLM gateway joins the dedicated networks of reasoning cages plus the
	// whole reasoning pool: every reasoning cage reaches it, no plain cage can.
	for _, key := range sortedStringKeys(plan.LLMAgents) {
		if net, ok := plan.AgentNets[key]; ok {
			plan.LLMNets = append(plan.LLMNets, net)
		}
	}
	plan.LLMNets = append(plan.LLMNets, plan.ReasonPool...)

	plan.MCPGatewayCfg.Record = ops.record
	cfgJSON, err := json.Marshal(plan.MCPGatewayCfg)
	if err != nil {
		return nil, fmt.Errorf("encoding MCP gateway routing table: %w", err)
	}
	// The MCP gateway joins every dedicated network plus both pools: the only
	// host that can reach every cage. Ordered for a deterministic arg list.
	mcpGatewayNets := make([]string, 0, len(plan.AgentNets)+len(plan.ReasonPool)+len(plan.PlainPool))
	for _, key := range sortedStringKeys(plan.AgentNets) {
		mcpGatewayNets = append(mcpGatewayNets, plan.AgentNets[key])
	}
	mcpGatewayNets = append(mcpGatewayNets, plan.ReasonPool...)
	mcpGatewayNets = append(mcpGatewayNets, plan.PlainPool...)
	plan.MCPGateway = ContainerSpec{
		RunID:    mcpGatewayName,
		ImageRef: GatewayImageRef(),
		Args:     []string{"mcp-gateway"},
		Networks: mcpGatewayNets,
		Env: map[string]string{
			env.MCPConfig: string(cfgJSON),
			env.MCPAddr:   ":" + env.DefaultMCPGatewayPort,
		},
		Detached: true,
		Managed:  ops.managed,
	}.withCap(defaultGatewayCap)
	return plan, nil
}

// classifyBans splits the root's BAN directives per node: wholeBanned agents
// never start, toolBanned tools merge into every edge reaching the agent. A
// BAN matches by registry and repository, so it takes out the agent whatever
// version a dependency pinned. Only the root's bans apply; the root itself is
// never a target.
func classifyBans(tree *runTree) (wholeBanned map[string]bool, toolBanned map[string][]string, err error) {
	wholeBanned = map[string]bool{}
	toolBanned = map[string][]string{}
	root := tree.Nodes[tree.Root].Manifest
	if root == nil {
		return wholeBanned, toolBanned, nil
	}
	for _, ban := range root.Agentfile.Ban {
		ref, err := reference.Parse(ban.Ref)
		if err != nil {
			return nil, nil, fmt.Errorf("BAN %s: %w", ban.Ref, err)
		}
		for key, node := range tree.Nodes {
			if key == tree.Root {
				continue
			}
			if node.Ref.Registry != ref.Registry || node.Ref.Repository != ref.Repository {
				continue
			}
			if len(ban.Tools) == 0 {
				wholeBanned[key] = true
			} else {
				toolBanned[key] = mergeDeny(toolBanned[key], ban.Tools)
			}
		}
	}
	return wholeBanned, toolBanned, nil
}

// mergeDeny unions two deny lists, dropping duplicates and preserving order.
func mergeDeny(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// agentImageRef is the local image ref a tree node builds into: the repository
// name tagged with the locked digest, so two pins of the same agent stay
// distinct and a built image is reused.
func agentImageRef(node *agentNode) string {
	name := node.Ref.Repository
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	tag := shortDigest(node.Ref.Digest)
	if tag == "" {
		tag = "build"
	}
	return "agentcage/" + sanitizeRef(name) + ":" + tag
}

// capabilityToken is the unguessable path a caller addresses one gateway route
// by. The gateway holds every route and authenticates no caller, so a
// predictable key would let any cage enumerate routes. The token goes only
// into the owning caller's injected URL; per-agent networks keep siblings from
// observing it.
func capabilityToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating capability token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func sortedNodeKeys(nodes map[string]*agentNode) []string {
	keys := make([]string, 0, len(nodes))
	for k := range nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
