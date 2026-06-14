package runtime

import "github.com/okedeji/agentcage/internal/config"

// operatorInputs bundles the per-run operator values the planner needs: the
// env and secret pools it injects, the per-agent model overrides, and the
// resource caps. It is loaded once per run from RunInput and the config file.
type operatorInputs struct {
	env       map[string]string
	secrets   map[string]string
	models    map[string]string
	resources config.Resources
}

// refKey is the config key for an agent: @org/name, version-independent, so an
// operator override survives a dependency bumping its pinned version. A node
// with no registry ref (a local root) has no key and takes no override.
func refKey(node *agentNode) string {
	if node == nil || node.Ref.Repository == "" {
		return ""
	}
	return "@" + node.Ref.Repository
}

// effectiveModel applies the operator's per-agent model override to an agent's
// advisory model. The override wins, so an operator can pin an expensive agent
// to a cheaper model for cost.
func effectiveModel(advisory string, node *agentNode, models map[string]string) string {
	if key := refKey(node); key != "" {
		if m, ok := models[key]; ok {
			return m
		}
	}
	return advisory
}

// agentCap resolves an agent cage's enforced cap fail-closed and per field:
// the operator's per-agent cap, then the operator default, then the runtime
// default. A partial operator cap still bounds every dimension.
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

// overlayCap layers over onto base per field, so a run's cap flags override the
// configured default only where they are set, leaving the rest in place.
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
