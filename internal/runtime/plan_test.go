package runtime

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/mcpgateway"
	"github.com/okedeji/agentcage/internal/reference"
)

func TestBuildRunPlan_RootCapAndModelHonorOperatorConfig(t *testing.T) {
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root": {
				Key:      "root",
				Ref:      reference.Reference{Repository: "okedeji/boss"},
				Manifest: &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Model: "openai/gpt-4o"}},
			},
		},
	}
	ops := operatorInputs{
		models: map[string]string{"@okedeji/boss": "openai/gpt-4o-mini"},
		resources: config.Resources{
			Defaults: config.Cap{CPUs: "8", Mem: "8g", Pids: 4096},
			Agents:   map[string]config.Cap{"@okedeji/boss": {Mem: "512m"}},
		},
	}
	plan, err := buildRunPlan(tree, "run1", ops)
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// The root is no longer special-cased to the runtime default: its per-agent
	// mem override wins, and cpus/pids fall through to the operator default.
	if plan.RootCap.Mem != "512m" || plan.RootCap.CPUs != "8" || plan.RootCap.Pids != 4096 {
		t.Errorf("RootCap = %+v, want mem 512m / cpus 8 / pids 4096", plan.RootCap)
	}
	if plan.LLMAgents["root"] != "openai/gpt-4o-mini" {
		t.Errorf("root model = %q, want the operator override openai/gpt-4o-mini", plan.LLMAgents["root"])
	}
}

func mustParseRef(t *testing.T, s string) reference.Reference {
	t.Helper()
	ref, err := reference.Parse(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return ref
}

func rootWithBans(bans ...bundle.BanSpec) *bundle.Manifest {
	return &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Ban: bans}}
}

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

	plan, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// The security property: the sub sits on its own network, distinct from the
	// root's, so the root cannot reach it directly. The gateway is the only host
	// on both, so it is the sole path between them.
	if plan.RootNet == "" || plan.AgentNets["sub-abc"] == "" {
		t.Fatalf("missing nets: root %q sub %q", plan.RootNet, plan.AgentNets["sub-abc"])
	}
	if plan.RootNet == plan.AgentNets["sub-abc"] {
		t.Errorf("root and sub share a network %q; they must be isolated", plan.RootNet)
	}
	if got := plan.Agents[0].Spec.Networks; len(got) != 1 || got[0] != plan.AgentNets["sub-abc"] {
		t.Errorf("sub networks = %v, want [%q]", got, plan.AgentNets["sub-abc"])
	}
	if !slices.Contains(plan.Gateway.Networks, plan.RootNet) || !slices.Contains(plan.Gateway.Networks, plan.AgentNets["sub-abc"]) {
		t.Errorf("gateway networks = %v, must include both the root and sub nets", plan.Gateway.Networks)
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
	// Every cage is capped. Sub-agents get the agent default, the gateway the
	// tighter gateway default, so none runs uncapped.
	if sub.Spec.Memory != defaultAgentCap.Mem || sub.Spec.Pids != defaultAgentCap.Pids {
		t.Errorf("sub cap = %q/%d, want %q/%d", sub.Spec.Memory, sub.Spec.Pids, defaultAgentCap.Mem, defaultAgentCap.Pids)
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

	if got := plan.Gateway.Env["AGENTCAGE_MCP_ADDR"]; got != ":9000" {
		t.Errorf("gateway addr = %q, want :9000", got)
	}
	if len(plan.Gateway.Args) != 1 || plan.Gateway.Args[0] != "mcp-gateway" {
		t.Errorf("gateway args = %v, want [mcp-gateway]", plan.Gateway.Args)
	}
	if plan.Gateway.Memory != defaultGatewayCap.Mem || plan.Gateway.Pids != defaultGatewayCap.Pids {
		t.Errorf("gateway cap = %q/%d, want %q/%d", plan.Gateway.Memory, plan.Gateway.Pids, defaultGatewayCap.Mem, defaultGatewayCap.Pids)
	}
	// The routing table the gateway serves round-trips back to what we
	// planned, so the container and the plan cannot disagree.
	var served mcpgateway.Config
	if err := json.Unmarshal([]byte(plan.Gateway.Env["AGENTCAGE_MCP_CONFIG"]), &served); err != nil {
		t.Fatalf("gateway config not valid json: %v", err)
	}
	if served.Edges["sub-0"].Target != edge.Target {
		t.Errorf("served edge target = %q, want %q", served.Edges["sub-0"].Target, edge.Target)
	}
}

func TestBuildRunPlan_InjectsLLMURLForReasoningAgents(t *testing.T) {
	withModel := func(m string, budget int64) *bundle.Manifest {
		return &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Model: m, Budget: budget}}
	}
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":   {Key: "root", Manifest: withModel("anthropic/claude-3.5", 5_000_000)},
			"sub-ab": {Key: "sub-ab", Manifest: withModel("openai/gpt-4o", 0)},
		},
		Edges: []usesEdge{{Caller: "root", Sub: "sub-ab", Alias: "sub"}},
	}

	plan, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// Each reasoning agent gets its own per-agent LLM URL and lands in the
	// gateway's per-agent model map; the root's budget becomes the run's.
	if got := plan.RootEnv["AGENTCAGE_LLM_URL"]; got != "http://run1-llm:9001/root" {
		t.Errorf("root LLM url = %q, want http://run1-llm:9001/root", got)
	}
	if plan.LLMAgents["root"] != "anthropic/claude-3.5" || plan.LLMAgents["sub-ab"] != "openai/gpt-4o" {
		t.Errorf("LLMAgents = %v", plan.LLMAgents)
	}
	if plan.Budget != 5_000_000 {
		t.Errorf("budget = %d, want 5000000", plan.Budget)
	}
	for _, a := range plan.Agents {
		if a.Spec.RunID == "run1-sub-ab" && a.Spec.Env["AGENTCAGE_LLM_URL"] != "http://run1-llm:9001/sub-ab" {
			t.Errorf("sub LLM url = %q", a.Spec.Env["AGENTCAGE_LLM_URL"])
		}
	}
}

func TestBuildRunPlan_EgressAllowAgentsGetProxyEnv(t *testing.T) {
	withEgress := func(policy string) *bundle.Manifest {
		return &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Egress: policy}}
	}
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":   {Key: "root", Manifest: withEgress("allow:api.openai.com")},
			"sub-ab": {Key: "sub-ab", Manifest: withEgress("allow:example.com, foo.test")},
			"sub-cd": {Key: "sub-cd", Manifest: withEgress("deny-default")},
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "sub-ab", Alias: "a"},
			{Caller: "root", Sub: "sub-cd", Alias: "c"},
		},
	}

	plan, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// Only the allow: agents are recorded, keyed by container name, and a
	// deny-default agent never appears, so the orchestrator never opens it a
	// route out. Each carries the network the proxy joins to reach it, and those
	// networks are distinct so the proxy keys each by its own address.
	root := plan.EgressAgents["run1"]
	if len(root.Hosts) != 1 || root.Hosts[0] != "api.openai.com" {
		t.Errorf("root egress hosts = %v, want [api.openai.com]", root.Hosts)
	}
	subAB := plan.EgressAgents["run1-sub-ab"]
	if len(subAB.Hosts) != 2 || subAB.Hosts[0] != "example.com" || subAB.Hosts[1] != "foo.test" {
		t.Errorf("sub-ab egress hosts = %v, want [example.com foo.test]", subAB.Hosts)
	}
	if root.Network == "" || subAB.Network == "" || root.Network == subAB.Network {
		t.Errorf("egress agent nets must be present and distinct: root %q sub-ab %q", root.Network, subAB.Network)
	}
	if _, ok := plan.EgressAgents["run1-sub-cd"]; ok {
		t.Errorf("deny-default agent must not be in EgressAgents: %v", plan.EgressAgents)
	}

	// The allow: agents are pointed at the proxy; intra-run gateways stay
	// direct so their plain-HTTP calls do not hit the CONNECT-only proxy.
	wantProxy := "http://run1-egress-proxy:9002"
	if got := plan.RootEnv["HTTPS_PROXY"]; got != wantProxy {
		t.Errorf("root HTTPS_PROXY = %q, want %q", got, wantProxy)
	}
	if got := plan.RootEnv["NO_PROXY"]; got != "run1-gw,run1-llm,localhost,127.0.0.1" {
		t.Errorf("root NO_PROXY = %q", got)
	}
	for _, a := range plan.Agents {
		switch a.Spec.RunID {
		case "run1-sub-ab":
			if a.Spec.Env["HTTP_PROXY"] != wantProxy {
				t.Errorf("sub-ab HTTP_PROXY = %q, want %q", a.Spec.Env["HTTP_PROXY"], wantProxy)
			}
		case "run1-sub-cd":
			if _, ok := a.Spec.Env["HTTP_PROXY"]; ok {
				t.Errorf("deny-default agent must get no proxy env: %v", a.Spec.Env)
			}
		}
	}
}

func TestBuildRunPlan_PerAgentNetworkIsolation(t *testing.T) {
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":  {Key: "root", Ref: mustParseRef(t, "@o/root:1.0"), Manifest: rootWithBans(bundle.BanSpec{Ref: "@o/bad"})},
			"web-1": {Key: "web-1", Ref: mustParseRef(t, "@o/web:1.0")},
			"bad-2": {Key: "bad-2", Ref: mustParseRef(t, "@o/bad:1.0")},
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "web-1", Alias: "web"},
			{Caller: "root", Sub: "bad-2", Alias: "bad"},
		},
	}

	plan, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// Every started agent sits on its own distinct network; no two share one.
	// This is the property that stops a hostile cage reaching a sibling.
	seen := map[string]string{}
	for key, net := range plan.AgentNets {
		if prev, ok := seen[net]; ok {
			t.Errorf("agents %s and %s share network %s", prev, key, net)
		}
		seen[net] = key
	}
	// The banned agent never starts, so it gets no network at all.
	if _, ok := plan.AgentNets["bad-2"]; ok {
		t.Error("banned agent bad-2 must have no network")
	}
	// The gateway is on every started agent's network and only those, so it is
	// the sole host that can reach all of them.
	if len(plan.Gateway.Networks) != len(plan.AgentNets) {
		t.Errorf("gateway nets = %v, want one per started agent %v", plan.Gateway.Networks, plan.AgentNets)
	}
	for key, net := range plan.AgentNets {
		if !slices.Contains(plan.Gateway.Networks, net) {
			t.Errorf("gateway missing net %s for agent %s", net, key)
		}
	}
}

func TestBuildRunPlan_WholeAgentBan(t *testing.T) {
	// root BANs @org/weird; it appears as a sub-agent and must not run, and
	// its edge must be rejected at the gateway.
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":     {Key: "root", Manifest: rootWithBans(bundle.BanSpec{Ref: "@org/weird"})},
			"weird-ab": {Key: "weird-ab", Ref: mustParseRef(t, "@org/weird:1.0")},
		},
		Edges: []usesEdge{{Caller: "root", Sub: "weird-ab", Alias: "weird"}},
	}

	plan, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	if len(plan.Agents) != 0 {
		t.Errorf("a banned agent was scheduled to start: %+v", plan.Agents)
	}
	edge, ok := plan.GatewayCfg.Edges["weird-0"]
	if !ok || !edge.Banned {
		t.Errorf("edge weird-0 should be banned, got %+v", edge)
	}
	// The URL is still injected so the caller gets a clean banned error.
	if plan.RootEnv["AGENTCAGE_USES_WEIRD_URL"] == "" {
		t.Error("banned edge should still inject the caller's URL")
	}
}

func TestBuildRunPlan_ToolBanMergesIntoEdgeDeny(t *testing.T) {
	// root BANs @org/web ONLY deep_crawl; web still runs, but every edge to
	// it denies deep_crawl on top of that edge's own DENY.
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":  {Key: "root", Manifest: rootWithBans(bundle.BanSpec{Ref: "@org/web", Tools: []string{"deep_crawl"}})},
			"web-1": {Key: "web-1", Ref: mustParseRef(t, "@org/web:2.0")},
		},
		Edges: []usesEdge{{Caller: "root", Sub: "web-1", Alias: "web", Deny: []string{"other"}}},
	}

	plan, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	if len(plan.Agents) != 1 {
		t.Fatalf("a tool-banned agent should still run, agents = %d", len(plan.Agents))
	}
	edge := plan.GatewayCfg.Edges["web-0"]
	if edge.Banned {
		t.Error("a tool ban must not mark the whole edge banned")
	}
	denies := map[string]bool{}
	for _, d := range edge.Deny {
		denies[d] = true
	}
	if !denies["other"] || !denies["deep_crawl"] {
		t.Errorf("edge deny = %v, want both the edge's own DENY and the subtree tool ban", edge.Deny)
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

	plan, err := buildRunPlan(tree, "run9", operatorInputs{})
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
