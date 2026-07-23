package runtime

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/mcpgateway"
	"github.com/okedeji/mcpvessel/internal/reference"
)

// edgeKeyFromURL pulls the gateway edge key out of an injected USES URL. The
// key is an opaque capability token, so tests derive it rather than assume a
// literal.
func edgeKeyFromURL(t *testing.T, url string) string {
	t.Helper()
	const suffix = "/mcp"
	trimmed, ok := strings.CutSuffix(url, suffix)
	if !ok {
		t.Fatalf("USES url %q has no /mcp suffix", url)
	}
	i := strings.LastIndexByte(trimmed, '/')
	if i < 0 {
		t.Fatalf("USES url %q has no edge key", url)
	}
	return trimmed[i+1:]
}

func TestBuildRunPlan_RootCapAndModelHonorOperatorConfig(t *testing.T) {
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root": {
				Key:      "root",
				Ref:      reference.Reference{Repository: "okedeji/boss"},
				Manifest: &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Model: "openai/gpt-4o"}},
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

	// The root is not special-cased to the runtime default: its per-agent
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
	return &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Ban: bans}}
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

	plan, err := buildRunPlan(tree, "run1", operatorInputs{maxLive: 32})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// The security property: the sub sits apart from the root's network. A
	// pooled cage (no MODEL) holds no dedicated network; it draws from the
	// plain pool at activation. The gateway is the sole path between them.
	if plan.RootNet == "" {
		t.Fatal("missing root net")
	}
	if _, dedicated := plan.AgentNets["sub-abc"]; dedicated {
		t.Error("a pooled sub-agent must not hold a dedicated network")
	}
	if got := plan.Agents[0].Spec.Networks; len(got) != 0 {
		t.Errorf("pooled sub networks = %v, want none in the plan (assigned at activation)", got)
	}
	if len(plan.PlainPool) != 1 {
		t.Fatalf("plain pool = %v, want one network for the single plain sub", plan.PlainPool)
	}
	if plan.PlainPool[0] == plan.RootNet {
		t.Error("pool network must be distinct from the root's")
	}
	if !slices.Contains(plan.MCPGateway.Networks, plan.RootNet) || !slices.Contains(plan.MCPGateway.Networks, plan.PlainPool[0]) {
		t.Errorf("gateway networks = %v, must include the root net and the plain pool", plan.MCPGateway.Networks)
	}

	// The root calls the gateway by an unguessable capability key, never the
	// sub-agent directly.
	url := plan.RootEnv["VESSEL_USES_SUB_URL"]
	if !strings.HasPrefix(url, "http://run1-gw:9000/") {
		t.Errorf("root USES url = %q, want the gateway prefix", url)
	}
	edgeKey := edgeKeyFromURL(t, url)
	if len(edgeKey) != 32 {
		t.Errorf("edge key = %q, want a 32-hex-char capability token, not a guessable alias-index", edgeKey)
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
	if got := sub.Spec.Env["VESSEL_SERVE_HTTP"]; got != ":8000" {
		t.Errorf("sub SERVE_HTTP = %q, want :8000", got)
	}
	// Every cage is capped: agent default for subs, tighter default for the gateway.
	if sub.Spec.Memory != defaultAgentCap.Mem || sub.Spec.Pids != defaultAgentCap.Pids {
		t.Errorf("sub cap = %q/%d, want %q/%d", sub.Spec.Memory, sub.Spec.Pids, defaultAgentCap.Mem, defaultAgentCap.Pids)
	}

	// The gateway routes the edge to the sub-agent's own container and
	// carries the deny list, so the referee is in the path.
	edge, ok := plan.MCPGatewayCfg.Edges[edgeKey]
	if !ok {
		t.Fatalf("no gateway edge %s in %+v", edgeKey, plan.MCPGatewayCfg.Edges)
	}
	if edge.Target != "http://run1-sub-abc:8000/mcp" {
		t.Errorf("edge target = %q, want http://run1-sub-abc:8000/mcp", edge.Target)
	}
	if len(edge.Deny) != 1 || edge.Deny[0] != "danger" {
		t.Errorf("edge deny = %v, want [danger]", edge.Deny)
	}

	if got := plan.MCPGateway.Env["VESSEL_MCP_ADDR"]; got != ":9000" {
		t.Errorf("gateway addr = %q, want :9000", got)
	}
	if len(plan.MCPGateway.Args) != 1 || plan.MCPGateway.Args[0] != "mcp-gateway" {
		t.Errorf("gateway args = %v, want [mcp-gateway]", plan.MCPGateway.Args)
	}
	if plan.MCPGateway.Memory != defaultGatewayCap.Mem || plan.MCPGateway.Pids != defaultGatewayCap.Pids {
		t.Errorf("gateway cap = %q/%d, want %q/%d", plan.MCPGateway.Memory, plan.MCPGateway.Pids, defaultGatewayCap.Mem, defaultGatewayCap.Pids)
	}
	// The routing table round-trips, so the container and the plan cannot
	// disagree. It carries capability tokens, so it is delivered off argv via
	// SecretEnv, not Env.
	if plan.MCPGateway.Env["VESSEL_MCP_CONFIG"] != "" {
		t.Error("MCP config must not be on argv (Env); it carries capability tokens")
	}
	var served mcpgateway.Config
	if err := json.Unmarshal([]byte(plan.MCPGateway.SecretEnv["VESSEL_MCP_CONFIG"]), &served); err != nil {
		t.Fatalf("gateway config not valid json: %v", err)
	}
	if served.Edges[edgeKey].Target != edge.Target {
		t.Errorf("served edge target = %q, want %q", served.Edges[edgeKey].Target, edge.Target)
	}
}

func TestBuildRunPlan_PinsEdgeToBuildTimeCatalog(t *testing.T) {
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root": {Key: "root"},
			"sub-abc": {Key: "sub-abc", Manifest: &bundle.Manifest{Tools: []bundle.Tool{
				{Name: "search", Visibility: bundle.VisibilityMain},
				{Name: "fetch", Visibility: bundle.VisibilityPrivate},
			}}},
			"sub-def": {Key: "sub-def"},
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "sub-abc", Alias: "cat"},
			{Caller: "root", Sub: "sub-def", Alias: "raw"},
		},
	}
	plan, err := buildRunPlan(tree, "run1", operatorInputs{maxLive: 32})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// A bundle with a recorded catalog gets it pinned as the edge's allow-set,
	// every visibility included: the parent sees what the build observed, not
	// what the running server later claims.
	catEdge := plan.MCPGatewayCfg.Edges[edgeKeyFromURL(t, plan.RootEnv["VESSEL_USES_CAT_URL"])]
	if got := strings.Join(catEdge.Allow, ","); got != "search,fetch" {
		t.Errorf("edge allow = %v, want [search fetch]", catEdge.Allow)
	}
	// No recorded catalog means no filter, not a ban of every tool.
	rawEdge := plan.MCPGatewayCfg.Edges[edgeKeyFromURL(t, plan.RootEnv["VESSEL_USES_RAW_URL"])]
	if rawEdge.Allow != nil {
		t.Errorf("edge allow with no catalog = %v, want nil", rawEdge.Allow)
	}
}

func TestBuildRunPlan_InjectsLLMURLForReasoningAgents(t *testing.T) {
	withModel := func(m string, budget int64) *bundle.Manifest {
		return &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Model: m, Budget: budget}}
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

	// Each reasoning agent's LLM URL carries its unguessable capability token,
	// not the guessable agent key, and lands in the gateway's per-agent map.
	rootURL := plan.RootEnv["VESSEL_LLM_URL"]
	if !strings.HasPrefix(rootURL, "http://run1-llm:9001/") {
		t.Errorf("root LLM url = %q, want the gateway prefix", rootURL)
	}
	rootTok := rootURL[strings.LastIndexByte(rootURL, '/')+1:]
	if rootTok != plan.LLMTokens["root"] || len(rootTok) != 32 {
		t.Errorf("root LLM token %q is not the unguessable capability token", rootTok)
	}
	if plan.LLMAgents["root"] != "anthropic/claude-3.5" || plan.LLMAgents["sub-ab"] != "openai/gpt-4o" {
		t.Errorf("LLMAgents = %v", plan.LLMAgents)
	}
	if plan.Budget != 5_000_000 {
		t.Errorf("budget = %d, want 5000000", plan.Budget)
	}
	for _, a := range plan.Agents {
		if a.Spec.RunID == "run1-sub-ab" {
			subURL := a.Spec.Env["VESSEL_LLM_URL"]
			tok := subURL[strings.LastIndexByte(subURL, '/')+1:]
			if tok != plan.LLMTokens["sub-ab"] || len(tok) != 32 {
				t.Errorf("sub LLM url = %q, token must be the capability token", subURL)
			}
		}
	}
}

func TestBuildRunPlan_SubAgentSecretsNeverHitArgv(t *testing.T) {
	// A sub-agent declaring a secret gets its value in the plan's SecretEnv
	// (fed off argv via --env-file), never in Env (which becomes --env argv).
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root": {Key: "root"},
			"sub-ab": {
				Key:      "sub-ab",
				Ref:      reference.Reference{Repository: "me/sub"},
				Manifest: &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Secrets: []string{"API_TOKEN"}}},
			},
		},
		Edges: []usesEdge{{Caller: "root", Sub: "sub-ab", Alias: "sub"}},
	}

	plan, err := buildRunPlan(tree, "run1", operatorInputs{
		maxLive: 32,
		secrets: Broadcast(map[string]string{"API_TOKEN": "sk-supersecret"}),
	})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}
	if len(plan.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(plan.Agents))
	}
	spec := plan.Agents[0].Spec
	if spec.SecretEnv["API_TOKEN"] != "sk-supersecret" {
		t.Errorf("secret not routed to SecretEnv: %+v", spec.SecretEnv)
	}
	if _, onArgv := spec.Env["API_TOKEN"]; onArgv {
		t.Error("secret value landed in Env, which becomes --env argv")
	}
	// The generated run args must not carry the value anywhere.
	if args := strings.Join(nerdctlRunArgs(spec), " "); strings.Contains(args, "sk-supersecret") {
		t.Errorf("secret value leaked onto the run args: %q", args)
	}
}

func TestBuildRunPlan_HostlessDefaultSubAgentStaysPooledAndUnproxied(t *testing.T) {
	// Regression: an imported sub-agent has no EGRESS directive (hold-capable
	// default) and no granted hosts. It used to land in EgressAgents with its
	// dedicated network name while staying pooled, so the egress proxy joined
	// a network that was never created and the whole run failed to boot with
	// "no such network". Such a node must stay poolable and get no proxy.
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":  {Key: "root", Manifest: &bundle.Manifest{}},
			"sub-1": {Key: "sub-1", Manifest: &bundle.Manifest{}},
		},
		Edges: []usesEdge{{Caller: "root", Sub: "sub-1", Alias: "s"}},
	}
	plan, err := buildRunPlan(tree, "run1", operatorInputs{maxLive: 8})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}
	if _, ok := plan.EgressAgents["run1-sub-1"]; ok {
		t.Errorf("hostless default sub-agent must not be in EgressAgents: %v", plan.EgressAgents)
	}
	for _, a := range plan.Agents {
		if a.Node.Key == "sub-1" {
			if a.AlwaysWarm {
				t.Error("hostless default sub-agent must stay poolable, not pinned warm")
			}
			if _, ok := a.Spec.Env["HTTPS_PROXY"]; ok {
				t.Error("hostless default sub-agent must not carry proxy env it has no route to")
			}
		}
	}

	// The moment the operator grants it a host (flag or config), it is proxied
	// and therefore pinned warm on its dedicated network.
	plan, err = buildRunPlan(tree, "run1", operatorInputs{maxLive: 8, egressAllow: []string{"api.example.com"}})
	if err != nil {
		t.Fatalf("buildRunPlan with egress: %v", err)
	}
	ea, ok := plan.EgressAgents["run1-sub-1"]
	if !ok || len(ea.Hosts) != 1 || ea.Hosts[0] != "api.example.com" {
		t.Errorf("granted sub-agent egress = %+v ok=%v, want [api.example.com]", ea, ok)
	}
	for _, a := range plan.Agents {
		if a.Node.Key == "sub-1" && !a.AlwaysWarm {
			t.Error("egress-bearing sub-agent must be pinned warm so its proxy network exists at boot")
		}
	}
}

func TestBuildRunPlan_EgressAllowAgentsGetProxyEnv(t *testing.T) {
	withEgress := func(policy string) *bundle.Manifest {
		return &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Egress: policy}}
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

	// Only allow: agents are recorded, keyed by container name; a deny-default
	// agent never appears, so it never gets a route out. The networks are
	// distinct so the proxy keys each agent by its own address.
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

func TestBuildRunPlan_EdgeKeysAreUnguessableCapabilities(t *testing.T) {
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root": {Key: "root"}, "a": {Key: "a"}, "b": {Key: "b"},
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "a", Alias: "x"},
			{Caller: "root", Sub: "b", Alias: "x"}, // same alias must not collide
		},
	}

	p1, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}
	if len(p1.MCPGatewayCfg.Edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(p1.MCPGatewayCfg.Edges))
	}
	for k := range p1.MCPGatewayCfg.Edges {
		// 32 hex chars, never the old guessable alias-index form a caller
		// could enumerate.
		if len(k) != 32 {
			t.Errorf("edge key %q is not a 32-hex capability token", k)
		}
		if strings.HasPrefix(k, "x-") {
			t.Errorf("edge key %q is the guessable alias-index form", k)
		}
	}

	// A fresh plan mints fresh tokens, so a key is not predictable across runs.
	p2, err := buildRunPlan(tree, "run1", operatorInputs{})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}
	for k := range p2.MCPGatewayCfg.Edges {
		if _, reused := p1.MCPGatewayCfg.Edges[k]; reused {
			t.Errorf("edge key %q reused across plans; tokens must be unpredictable", k)
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

	plan, err := buildRunPlan(tree, "run1", operatorInputs{maxLive: 32})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// Every network in the run is distinct; the pool is reused only
	// sequentially. This is what stops a hostile cage reaching a sibling.
	seen := map[string]bool{}
	allNets := append([]string{}, plan.ReasonPool...)
	allNets = append(allNets, plan.PlainPool...)
	for _, net := range plan.AgentNets {
		allNets = append(allNets, net)
	}
	for _, net := range allNets {
		if seen[net] {
			t.Errorf("network %s appears twice; every network must be distinct", net)
		}
		seen[net] = true
	}
	// web-1 is pooled: no dedicated network, one plain pool network to draw.
	// The banned bad-2 never starts, so it gets no network anywhere.
	if _, ok := plan.AgentNets["web-1"]; ok {
		t.Error("pooled agent web-1 must hold no dedicated network")
	}
	if len(plan.PlainPool) != 1 {
		t.Errorf("plain pool = %v, want one network for the single pooled cage", plan.PlainPool)
	}
	// The gateway joins every network, so it alone reaches every cage.
	for _, net := range allNets {
		if !slices.Contains(plan.MCPGateway.Networks, net) {
			t.Errorf("gateway missing net %s", net)
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
	// The URL is still injected so the caller gets a clean banned error.
	if plan.RootEnv["VESSEL_USES_WEIRD_URL"] == "" {
		t.Fatal("banned edge should still inject the caller's URL")
	}
	edgeKey := edgeKeyFromURL(t, plan.RootEnv["VESSEL_USES_WEIRD_URL"])
	edge, ok := plan.MCPGatewayCfg.Edges[edgeKey]
	if !ok || !edge.Banned {
		t.Errorf("edge %s should be banned, got %+v", edgeKey, edge)
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
	edge := plan.MCPGatewayCfg.Edges[edgeKeyFromURL(t, plan.RootEnv["VESSEL_USES_WEB_URL"])]
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

func TestBuildRunPlan_PrewarmsDirectChildrenDefersDeeper(t *testing.T) {
	// root -> a -> b. The skeleton boots a (a direct child); b activates on
	// first call, so the root's edge to a is active and a's edge to b is inactive.
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

	plan, err := buildRunPlan(tree, "run1", operatorInputs{prewarm: 8})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	// The direct child prewarms; the grandchild does not.
	for _, a := range plan.Agents {
		switch a.Node.Key {
		case "a-111":
			if !a.Prewarm {
				t.Error("direct child a-111 should prewarm")
			}
		case "b-222":
			if a.Prewarm {
				t.Error("grandchild b-222 should not prewarm")
			}
		}
	}

	// The direct child's edge is live from boot; the deeper edge is inactive
	// so the gateway holds its first call.
	rootEdge := plan.MCPGatewayCfg.Edges[edgeKeyFromURL(t, plan.RootEnv["VESSEL_USES_A_URL"])]
	if rootEdge.Inactive {
		t.Error("edge to a prewarmed direct child must be active")
	}
	var aAgent plannedAgent
	for _, ag := range plan.Agents {
		if ag.Node.Key == "a-111" {
			aAgent = ag
		}
	}
	deepEdgeKey := edgeKeyFromURL(t, aAgent.Spec.Env["VESSEL_USES_B_URL"])
	if !plan.MCPGatewayCfg.Edges[deepEdgeKey].Inactive {
		t.Error("edge to the deferred grandchild b must be inactive")
	}

	// EdgeNodes folds an edge key back to the node the activation manager boots.
	if plan.EdgeNodes[deepEdgeKey] != "b-222" {
		t.Errorf("EdgeNodes[%s] = %q, want b-222", deepEdgeKey, plan.EdgeNodes[deepEdgeKey])
	}
}

func TestBuildRunPlan_TwoNetworkPoolsKeepKeyHolderOffPlainCages(t *testing.T) {
	withModel := func(key, m string) *agentNode {
		return &agentNode{Key: key, Manifest: &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Model: m}}}
	}
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":      {Key: "root"},
			"reasoner":  withModel("reasoner", "openai/gpt-4o"),
			"collector": {Key: "collector"}, // no MODEL: a plain tool collection
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "reasoner", Alias: "r"},
			{Caller: "root", Sub: "collector", Alias: "c"},
		},
	}

	// prewarm 0 so both subs are pooled and lazy, exercising the pools directly.
	plan, err := buildRunPlan(tree, "run1", operatorInputs{maxLive: 32})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	if len(plan.ReasonPool) != 1 || len(plan.PlainPool) != 1 {
		t.Fatalf("pools = reason %v / plain %v, want one each", plan.ReasonPool, plan.PlainPool)
	}
	// Pooled cages hold no network in the plan; they draw one at activation.
	for _, a := range plan.Agents {
		if len(a.Spec.Networks) != 0 {
			t.Errorf("pooled cage %s has a baked network %v", a.Node.Key, a.Spec.Networks)
		}
	}
	// The LLM gateway joins the reasoning pool, never the plain pool: a
	// non-reasoning cage must not share a network with the key holder.
	if !slices.Contains(plan.LLMNets, plan.ReasonPool[0]) {
		t.Errorf("LLM gateway must join the reasoning pool, got %v", plan.LLMNets)
	}
	if slices.Contains(plan.LLMNets, plan.PlainPool[0]) {
		t.Errorf("LLM gateway must NOT join the plain pool, got %v", plan.LLMNets)
	}
	// The MCP gateway joins both pools, so it reaches every cage.
	if !slices.Contains(plan.MCPGateway.Networks, plan.ReasonPool[0]) || !slices.Contains(plan.MCPGateway.Networks, plan.PlainPool[0]) {
		t.Errorf("MCP gateway must join both pools, got %v", plan.MCPGateway.Networks)
	}
}

func TestBuildRunPlan_PinsEgressAndConfiguredAgentsWarm(t *testing.T) {
	// root -> a -> deep. deep declares EGRESS allow: and the operator pins
	// @o/a keep_warm; both must prewarm and be AlwaysWarm even though deep is
	// not a direct child.
	withEgress := func(ref reference.Reference, policy string) *agentNode {
		return &agentNode{Key: "deep-1", Ref: ref, Manifest: &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Egress: policy}}}
	}
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":   {Key: "root"},
			"a-1":    {Key: "a-1", Ref: mustParseRef(t, "@o/a:1.0")},
			"deep-1": withEgress(mustParseRef(t, "@o/deep:1.0"), "allow:example.com"),
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "a-1", Alias: "a"},
			{Caller: "a-1", Sub: "deep-1", Alias: "deep"},
		},
	}

	plan, err := buildRunPlan(tree, "run1", operatorInputs{prewarm: 8, keepWarm: []string{"@o/a"}})
	if err != nil {
		t.Fatalf("buildRunPlan: %v", err)
	}

	flags := map[string]plannedAgent{}
	for _, a := range plan.Agents {
		flags[a.Node.Key] = a
	}
	if !flags["deep-1"].Prewarm || !flags["deep-1"].AlwaysWarm {
		t.Errorf("egress agent deep-1 must be prewarmed and always-warm, got %+v", flags["deep-1"])
	}
	if !flags["a-1"].Prewarm || !flags["a-1"].AlwaysWarm {
		t.Errorf("config-pinned a-1 must be prewarmed and always-warm, got %+v", flags["a-1"])
	}
	// The deep agent is warm, so the edge to it is active, not held for activation.
	deepEdge := edgeKeyFromURL(t, flags["a-1"].Spec.Env["VESSEL_USES_DEEP_URL"])
	if plan.MCPGatewayCfg.Edges[deepEdge].Inactive {
		t.Error("edge to a pinned-warm agent must be active")
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
	if got := a.Spec.Env["VESSEL_SERVE_HTTP"]; got != ":8000" {
		t.Errorf("a SERVE_HTTP = %q, want :8000", got)
	}
	// a's url for b points at the gateway by capability key, not at b's
	// container, so a's own deny edge to b is enforced.
	gotURL := a.Spec.Env["VESSEL_USES_B_URL"]
	if !strings.HasPrefix(gotURL, "http://run9-gw:9000/") {
		t.Errorf("a USES b url = %q, want the gateway prefix", gotURL)
	}
	if _, ok := plan.MCPGatewayCfg.Edges[edgeKeyFromURL(t, gotURL)]; !ok {
		t.Errorf("a USES b url %q does not resolve to a gateway edge", gotURL)
	}
	// The nested edge is the gateway's, not the root's.
	if _, isRoot := plan.RootEnv["VESSEL_USES_B_URL"]; isRoot {
		t.Error("b leaked into the root's environment")
	}
}
