package runtime

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/gateway"
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

// runPlan is everything the orchestrator needs to start a USES tree: the
// per-run network, a detached container spec for each non-root agent and
// for the gateway, the gateway's routing table, and the sub-agent URLs the
// root parent gets injected. It is derived purely from the resolved tree,
// so the security-load-bearing wiring (which edge denies what, which URL
// reaches which sub) is unit tested without starting a container.
type runPlan struct {
	Network    string
	GatewayCfg gateway.Config
	Gateway    ContainerSpec
	Agents     []plannedAgent
	RootEnv    map[string]string
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
func buildRunPlan(tree *runTree, runID string) (*runPlan, error) {
	network := runID + "-net"
	gatewayName := runID + "-gw"

	containerName := func(key string) string {
		if key == tree.Root {
			return runID
		}
		return runID + "-" + key
	}

	plan := &runPlan{
		Network:    network,
		GatewayCfg: gateway.Config{Edges: map[string]gateway.Edge{}},
		RootEnv:    map[string]string{},
	}

	// callerEnv collects the sub-agent URLs each non-root caller is injected
	// with; the root's go straight into plan.RootEnv.
	callerEnv := map[string]map[string]string{}
	for i, e := range tree.Edges {
		edgeKey := fmt.Sprintf("%s-%d", sanitizeRef(e.Alias), i)
		plan.GatewayCfg.Edges[edgeKey] = gateway.Edge{
			Target: "http://" + containerName(e.Sub) + ":" + agentServePort + mcpServePath,
			Deny:   e.Deny,
		}

		url := "http://" + gatewayName + ":" + env.DefaultGatewayPort + "/" + edgeKey + mcpServePath
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
		if key == tree.Root {
			continue
		}
		agentEnv := map[string]string{env.ServeHTTP: ":" + agentServePort}
		for k, v := range callerEnv[key] {
			agentEnv[k] = v
		}
		plan.Agents = append(plan.Agents, plannedAgent{
			Node: tree.Nodes[key],
			Spec: ContainerSpec{
				RunID:    containerName(key),
				ImageRef: agentImageRef(key),
				Network:  network,
				Env:      agentEnv,
				Detached: true,
			},
		})
	}

	cfgJSON, err := json.Marshal(plan.GatewayCfg)
	if err != nil {
		return nil, fmt.Errorf("encoding gateway routing table: %w", err)
	}
	plan.Gateway = ContainerSpec{
		RunID:    gatewayName,
		ImageRef: GatewayImageRef(),
		Network:  network,
		Env: map[string]string{
			env.GatewayConfig: string(cfgJSON),
			env.GatewayAddr:   ":" + env.DefaultGatewayPort,
		},
		Detached: true,
	}
	return plan, nil
}

// agentImageRef is the local image tag a tree node builds into. Keyed by
// the run-unique node key so two pins of the same agent name stay distinct
// in containerd's image store.
func agentImageRef(key string) string {
	return "agentcage/" + sanitizeRef(key) + ":latest"
}

func sortedNodeKeys(nodes map[string]*agentNode) []string {
	keys := make([]string, 0, len(nodes))
	for k := range nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
