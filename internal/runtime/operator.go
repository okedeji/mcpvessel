package runtime

import (
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/reference"
)

// operatorInputs carries the per-run operator values the planner needs,
// loaded once per run from RunInput and the config file.
type operatorInputs struct {
	env map[string]string
	// secrets is scoped: the broadcast pool plus per-agent grants keyed by
	// the run name (root) or a USES alias (sub-agents).
	secrets ScopedSecrets
	// rootName is the root agent's secret scope, the resolved run name.
	rootName  string
	models    map[string]string
	resources config.Resources
	// managed labels planned containers so a restarted daemon can sweep orphans.
	managed bool
	// prewarm caps how many of the root's direct children the skeleton boots;
	// the rest activate on first call. Zero prewarms nothing; the resolver
	// applies the runtime default, so the daemon never passes zero.
	prewarm int
	// keepWarm lists agent refs (@org/name) booted with the skeleton and never
	// reaped or evicted.
	keepWarm []string
	// maxLive caps live cages per run; the planner also sizes each network
	// pool by it, since that is the most networks of one pool a run can hold.
	maxLive int
	// record turns on the MCP gateway's full-payload capture for replay.
	record bool
	// egressAllow is the operator's per-run egress override for the root agent,
	// added on top of what its Vesselfile declares.
	egressAllow []string
	// configEgress is the operator's persisted egress allow-lists (general and
	// per-agent), resolved per node by ref and unioned on top of everything
	// above. This is where an interactive `egress allow` approval is remembered.
	configEgress config.EgressPolicy
}

// refKey is the config key for an agent: @org/name, version-independent so an
// override survives a dependency version bump. A local root has no registry
// ref, no key, and takes no override.
func refKey(node *agentNode) string {
	if node == nil || node.Ref.Repository == "" {
		return ""
	}
	return "@" + node.Ref.Repository
}

// tagKey is the version-specific config key for an agent, @org/name:version.
// Unlike refKey it keeps the version, so an operator's egress or secret binding
// applies to the exact version they approved and a version bump asks again. A
// node without a registry ref or version has no tag key.
func tagKey(node *agentNode) string {
	if node == nil || node.Ref.Repository == "" || node.Ref.Tag == "" {
		return ""
	}
	return "@" + node.Ref.Repository + ":" + node.Ref.Tag
}

// configEgressFor resolves the operator's persisted egress hosts for a node:
// the general default, the any-version key, and the exact-version key.
func configEgressFor(node *agentNode, p config.EgressPolicy) []string {
	return p.For(tagKey(node), refKey(node))
}

// configEgressForRef resolves persisted egress hosts for a ref string, for the
// single-cage path that has no tree node. An unparsable or local ref still
// picks up the general default.
func configEgressForRef(ref string, p config.EgressPolicy) []string {
	r, err := reference.Parse(ref)
	if err != nil || r.Repository == "" {
		return p.For("", "")
	}
	nameKey := "@" + r.Repository
	versionKey := ""
	if r.Tag != "" {
		versionKey = nameKey + ":" + r.Tag
	}
	return p.For(versionKey, nameKey)
}

// effectiveModel returns the operator's per-agent model override if set,
// otherwise the agent's advisory model.
func effectiveModel(advisory string, node *agentNode, models map[string]string) string {
	if key := refKey(node); key != "" {
		if m, ok := models[key]; ok {
			return m
		}
	}
	return advisory
}

// agentCap resolves a cage's enforced cap per field: per-agent cap, then
// operator default, then runtime default. Fail-closed: a partial operator
// cap still bounds every dimension.
func agentCap(node *agentNode, res config.Resources) config.Cap {
	var perAgent config.Cap
	if key := refKey(node); key != "" {
		perAgent = res.Agents[key]
	}
	return config.Cap{
		CPUs: firstNonEmpty(perAgent.CPUs, res.Defaults.CPUs, defaultAgentCap.CPUs),
		Mem:  firstNonEmpty(perAgent.Mem, res.Defaults.Mem, defaultAgentCap.Mem),
		Pids: firstNonZero(perAgent.Pids, res.Defaults.Pids, defaultAgentCap.Pids),
	}
}

// overlayCap layers over onto base per field; unset fields keep base.
func overlayCap(over, base config.Cap) config.Cap {
	return config.Cap{
		CPUs: firstNonEmpty(over.CPUs, base.CPUs),
		Mem:  firstNonEmpty(over.Mem, base.Mem),
		Pids: firstNonZero(over.Pids, base.Pids),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
