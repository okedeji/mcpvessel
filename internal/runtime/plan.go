package runtime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/mcpgateway"
	"github.com/okedeji/agentcage/internal/reference"
)

const (
	// agentServePort is the port every sub-agent serves streamable-HTTP on
	// inside the run network. Each agent is its own container, so they all
	// share one port without colliding.
	agentServePort = "8000"

	// mcpServePath is where a sub-agent serves MCP and where the gateway
	// forwards an edge to. Both sides have to agree: the agent binds it,
	// the gateway targets it.
	mcpServePath = "/mcp"
)

// runPlan is everything the orchestrator needs to start a USES tree: a network
// per agent, a detached container spec for each non-root agent and for the
// gateway, the gateway's routing table, and the sub-agent URLs the root parent
// gets injected. It is derived purely from the resolved tree, so the
// security-load-bearing wiring (which agent sits on which network, which edge
// denies what) is unit tested without starting a container.
type runPlan struct {
	GatewayCfg mcpgateway.Config
	Gateway    ContainerSpec
	Agents     []plannedAgent
	RootEnv    map[string]string

	// AgentNets maps each started agent's key (the root included) to its own
	// internal network, shared only with the gateways. A banned agent never
	// starts, so it has no entry and no network. RootNet is the attached root's,
	// carried out because the root starts outside the sub-agent loop. Each agent
	// alone on its own network is what stops a cage from reaching a sibling
	// directly and bypassing the gateway's deny.
	AgentNets map[string]string
	RootNet   string

	// LLMAgents maps each reasoning agent's key to its advisory model, the
	// per-agent map the LLM gateway routes by. Empty when nothing in the tree
	// reasons, which tells the orchestrator no LLM gateway is needed. Budget is
	// the root's advisory cap in micro-USD, the run's shared pool unless the
	// operator overrides it.
	LLMAgents map[string]string
	Budget    int64

	// EgressAgents maps each allow: agent's container name to its network and
	// the hosts it may reach. The egress proxy multi-homes onto each network and
	// keys its allow-list by the agent's address on it. Empty when nothing in
	// the tree declares allow:, which tells the orchestrator no proxy is needed.
	EgressAgents map[string]egressAgent

	// RootCap is the attached root's resolved resource cap. The root runs
	// outside the sub-agent loop, so its cap is resolved here too rather than
	// left to the runtime default, which would silently ignore the operator.
	RootCap config.Cap
}

// egressAgent is one allow: agent's egress wiring: the network the proxy joins
// to reach it, and the hosts it may reach.
type egressAgent struct {
	Network string
	Hosts   []string
}

// plannedAgent pairs a non-root tree node with the detached container spec
// that runs it. The node carries the bundle and manifest the orchestrator
// builds the image from.
type plannedAgent struct {
	Node *agentNode
	Spec ContainerSpec
}

// buildRunPlan turns a resolved tree into the containers, network, and
// gateway routing for a run. runID scopes every name so concurrent runs do
// not collide. Each USES edge becomes one gateway route (target plus deny)
// and one injected AGENTCAGE_USES_<ALIAS>_URL on the caller pointing at the
// gateway, so every call in the tree passes the referee that enforces deny.
func buildRunPlan(tree *runTree, runID string, ops operatorInputs) (*runPlan, error) {
	gatewayName := runID + "-gw"

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
		GatewayCfg:   mcpgateway.Config{Edges: map[string]mcpgateway.Edge{}},
		RootEnv:      map[string]string{},
		AgentNets:    map[string]string{},
		LLMAgents:    map[string]string{},
		EgressAgents: map[string]egressAgent{},
		Budget:       nodeBudget(tree.Nodes[tree.Root]),
	}

	wholeBanned, toolBanned, err := classifyBans(tree)
	if err != nil {
		return nil, err
	}

	// callerEnv collects the sub-agent URLs each non-root caller is injected
	// with; the root's go straight into plan.RootEnv.
	callerEnv := map[string]map[string]string{}
	for i, e := range tree.Edges {
		edgeKey := fmt.Sprintf("%s-%d", sanitizeRef(e.Alias), i)
		edge := mcpgateway.Edge{
			Target: "http://" + containerName(e.Sub) + ":" + agentServePort + mcpServePath,
		}
		if wholeBanned[e.Sub] {
			edge.Banned = true
		} else {
			edge.Deny = mergeDeny(e.Deny, toolBanned[e.Sub])
		}
		plan.GatewayCfg.Edges[edgeKey] = edge

		// A banned edge still gets its URL injected, so the caller reaches the
		// gateway and gets a clean banned error rather than a missing variable.
		url := "http://" + gatewayName + ":" + env.DefaultMCPGatewayPort + "/" + edgeKey + mcpServePath
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
			// A banned agent never starts; its edges are rejected at the gateway.
			continue
		}
		agentEnv := map[string]string{env.ServeHTTP: ":" + agentServePort}
		for k, v := range callerEnv[key] {
			agentEnv[k] = v
		}
		node := tree.Nodes[key]
		plan.AgentNets[key] = nodeNet(key)
		if model := nodeModel(node); model != "" {
			agentEnv[env.LLMURL] = llmURL(runID, key)
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
				Networks: []string{nodeNet(key)},
				Env:      agentEnv,
				Detached: true,
			}.withCap(agentCap(node, ops.resources)),
		})
	}

	// The root reasons over its own LLM URL too. It is not in the sub-agent
	// loop above (it runs attached, not detached), so inject it here.
	plan.AgentNets[tree.Root] = nodeNet(tree.Root)
	plan.RootNet = nodeNet(tree.Root)
	if model := nodeModel(tree.Nodes[tree.Root]); model != "" {
		plan.RootEnv[env.LLMURL] = llmURL(runID, tree.Root)
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

	cfgJSON, err := json.Marshal(plan.GatewayCfg)
	if err != nil {
		return nil, fmt.Errorf("encoding gateway routing table: %w", err)
	}
	// The gateway joins every started agent's network, so it is the only host
	// that can reach all of them and the only one a caller resolves its USES
	// URLs to. Ordered by agent key for a deterministic, testable arg list.
	gatewayNets := make([]string, 0, len(plan.AgentNets))
	for _, key := range sortedStringKeys(plan.AgentNets) {
		gatewayNets = append(gatewayNets, plan.AgentNets[key])
	}
	plan.Gateway = ContainerSpec{
		RunID:    gatewayName,
		ImageRef: GatewayImageRef(),
		Args:     []string{"mcp-gateway"},
		Networks: gatewayNets,
		Env: map[string]string{
			env.MCPConfig: string(cfgJSON),
			env.MCPAddr:   ":" + env.DefaultMCPGatewayPort,
		},
		Detached: true,
	}.withCap(defaultGatewayCap)
	return plan, nil
}

// classifyBans reads the root's BAN directives and splits them per node:
// wholeBanned names agents banned outright (not started, every edge to them
// rejected); toolBanned names the tools to merge into every edge reaching an
// agent. A BAN matches a node when the pinned ref shares its registry and
// repository, so a ban takes out the agent whatever version a dependency
// pinned. Only the root's bans apply, and the root itself is never a target.
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

// agentImageRef is the local containerd image ref a tree node builds into:
// its name from the agent's repository, its tag the locked digest. The
// digest in the tag keeps two pins of the same agent distinct and lets an
// already-built image be reused or skipped.
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

func sortedNodeKeys(nodes map[string]*agentNode) []string {
	keys := make([]string, 0, len(nodes))
	for k := range nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
