package runtime

import (
	"encoding/json"
	"testing"

	"github.com/okedeji/agentcage/internal/gateway"
)

func TestBuildRunPlan_SingleEdge(t *testing.T) {
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":    {Key: "root"},
			"sub-abc": {Key: "sub-abc"},
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "sub-abc", Alias: "sub", Deny: []string{"danger"}},
		},
	}

	plan, err := buildRunPlan(tree, "run1")
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	if plan.Network != "run1-net" {
		t.Errorf("network = %q, want run1-net", plan.Network)
	}

	// The root calls the gateway, never the sub-agent directly.
	wantURL := "http://run1-gw:9000/sub-0/mcp"
	if got := plan.RootEnv["AGENTCAGE_USES_SUB_URL"]; got != wantURL {
		t.Errorf("root USES url = %q, want %q", got, wantURL)
	}

	if len(plan.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(plan.Agents))
	}
	sub := plan.Agents[0]
	if sub.Spec.RunID != "run1-sub-abc" {
		t.Errorf("sub container = %q, want run1-sub-abc", sub.Spec.RunID)
	}
	if !sub.Spec.Detached {
		t.Error("sub-agent must run detached")
	}
	if got := sub.Spec.Env["AGENTCAGE_SERVE_HTTP"]; got != ":8000" {
		t.Errorf("sub SERVE_HTTP = %q, want :8000", got)
	}

	// The gateway routes the edge to the sub-agent's own container and
	// carries the deny list, so the referee is in the path.
	edge, ok := plan.GatewayCfg.Edges["sub-0"]
	if !ok {
		t.Fatalf("no gateway edge sub-0 in %+v", plan.GatewayCfg.Edges)
	}
	if edge.Target != "http://run1-sub-abc:8000/mcp" {
		t.Errorf("edge target = %q, want http://run1-sub-abc:8000/mcp", edge.Target)
	}
	if len(edge.Deny) != 1 || edge.Deny[0] != "danger" {
		t.Errorf("edge deny = %v, want [danger]", edge.Deny)
	}

	if got := plan.Gateway.Env["AGENTCAGE_GATEWAY_ADDR"]; got != ":9000" {
		t.Errorf("gateway addr = %q, want :9000", got)
	}
	// The routing table the gateway serves round-trips back to what we
	// planned, so the container and the plan cannot disagree.
	var served gateway.Config
	if err := json.Unmarshal([]byte(plan.Gateway.Env["AGENTCAGE_GATEWAY_CONFIG"]), &served); err != nil {
		t.Fatalf("gateway config not valid json: %v", err)
	}
	if served.Edges["sub-0"].Target != edge.Target {
		t.Errorf("served edge target = %q, want %q", served.Edges["sub-0"].Target, edge.Target)
	}
}

func TestBuildRunPlan_NestedCallerServesAndCalls(t *testing.T) {
	// root -> a -> b. Agent a is both a server (root reaches it) and a
	// caller (it reaches b), so its env carries SERVE_HTTP and a USES url.
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":  {Key: "root"},
			"a-111": {Key: "a-111"},
			"b-222": {Key: "b-222"},
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "a-111", Alias: "a"},
			{Caller: "a-111", Sub: "b-222", Alias: "b"},
		},
	}

	plan, err := buildRunPlan(tree, "run9")
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	var a plannedAgent
	for _, ag := range plan.Agents {
		if ag.Spec.RunID == "run9-a-111" {
			a = ag
		}
	}
	if a.Node == nil {
		t.Fatal("agent a not in plan")
	}
	if got := a.Spec.Env["AGENTCAGE_SERVE_HTTP"]; got != ":8000" {
		t.Errorf("a SERVE_HTTP = %q, want :8000", got)
	}
	// a's url for b points at the gateway, not at b's container, so a's
	// own deny edge to b is enforced.
	if got := a.Spec.Env["AGENTCAGE_USES_B_URL"]; got != "http://run9-gw:9000/b-1/mcp" {
		t.Errorf("a USES b url = %q, want http://run9-gw:9000/b-1/mcp", got)
	}
	// The nested edge is the gateway's, not the root's.
	if _, isRoot := plan.RootEnv["AGENTCAGE_USES_B_URL"]; isRoot {
		t.Error("b leaked into the root's environment")
	}
}
