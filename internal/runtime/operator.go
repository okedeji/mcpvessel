package runtime

import "github.com/okedeji/agentcage/internal/config"

// operatorInputs carries the per-run operator values the planner needs,
// loaded once per run from RunInput and the config file.
type operatorInputs struct {
	env       map[string]string
	secrets   map[string]string
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
