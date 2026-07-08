package runtime

import (
	"fmt"
	"io"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
)

// usableMemory applies machine.memory_gib to the machine's real memory,
// warning when the setting asks for more than the machine has.
func usableMemory(p Provisioner, machineMemCap int64, stderr io.Writer) (int64, error) {
	avail, err := p.AvailableMemory()
	if err != nil {
		return 0, fmt.Errorf("reading machine memory: %w", err)
	}
	usable, overRequest := effectiveAvailable(avail, machineMemCap)
	if overRequest {
		_, _ = fmt.Fprintf(stderr, "note: machine.memory_gib requests %s but the machine has %s; recreate the VM to apply on macOS, or lower it below host RAM on Linux. Using %s\n",
			HumanBytes(machineMemCap), HumanBytes(avail), HumanBytes(avail))
	}
	return usable, nil
}

// soloBaselineMemory is a single-cage run's always-on memory: the cage, plus
// the LLM gateway if it reasons and the egress proxy if it declares allow:.
// No MCP gateway, a lone agent has no USES tree.
func soloBaselineMemory(rootMem int64, reasons, egress bool) int64 {
	total := rootMem
	gw := defaultGatewayCap.MemBytes()
	if reasons {
		total += gw
	}
	if egress {
		total += gw
	}
	return total
}

// CageMemoryBytes is the memory one cage gets: its manifest's RESOURCES hint,
// or the runtime default when it states none.
func CageMemoryBytes(m *bundle.Manifest) int64 {
	if m != nil && m.Agentfile.Resources != nil {
		if b := (config.Cap{Mem: m.Agentfile.Resources.Mem}).MemBytes(); b > 0 {
			return b
		}
	}
	return defaultAgentCap.MemBytes()
}

// treeBaselineMemory is the always-on memory a tree run needs: the root cage,
// the gateway singletons the tree requires, and the egress sub-agent cages
// (compulsory, the egress proxy keys them by a stable IP). Elastic sub-agents
// activate on demand and are not counted.
func treeBaselineMemory(tree *runTree) int64 {
	total := CageMemoryBytes(tree.Nodes[tree.Root].Manifest)
	gw := defaultGatewayCap.MemBytes()
	if len(tree.Edges) > 0 {
		total += gw // MCP gateway, present in any USES tree
	}
	reasons, egress := false, false
	for _, n := range tree.Nodes {
		if nodeModel(n) != "" {
			reasons = true
		}
		if len(egressHosts(nodeEgress(n))) > 0 {
			egress = true
		}
	}
	if reasons {
		total += gw
	}
	if egress {
		total += gw
	}
	for key, n := range tree.Nodes {
		if key == tree.Root {
			continue
		}
		if len(egressHosts(nodeEgress(n))) > 0 {
			total += CageMemoryBytes(n.Manifest)
		}
	}
	return total
}

// hostMemoryReserve is held back for the kernel, containerd, and buildkitd.
// A flat gibibyte deliberately over-estimates: better to refuse a run that
// would have just fit than to OOM the host mid-run.
const hostMemoryReserve = 1 << 30

// compulsoryMemory sums the run's always-on cages: root, required gateway
// singletons, kept-warm sub-agents. A BAN shrinks this (the agent is absent
// from plan.Agents); a tool-level DENY does not, the agent still runs.
func compulsoryMemory(plan *runPlan) int64 {
	total := plan.RootCap.MemBytes()

	gw := defaultGatewayCap.MemBytes()
	total += gw // MCP gateway, always present in a USES tree
	if len(plan.LLMAgents) > 0 {
		total += gw
	}
	if len(plan.EgressAgents) > 0 {
		total += gw
	}

	for _, a := range plan.Agents {
		if a.AlwaysWarm {
			total += config.Cap{Mem: a.Spec.Memory}.MemBytes()
		}
	}
	return total
}

// maxElasticMem is the largest cap among the run's elastic cages, the worst
// case one activation can take.
func maxElasticMem(plan *runPlan) int64 {
	var max int64
	for _, a := range plan.Agents {
		if a.AlwaysWarm {
			continue
		}
		if m := (config.Cap{Mem: a.Spec.Memory}).MemBytes(); m > max {
			max = m
		}
	}
	return max
}

// effectiveAvailable applies machine.memory_gib to the real memory. A setting
// below it caps capacity; a setting above it is ignored and flagged true.
// Zero means no setting.
func effectiveAvailable(avail, configured int64) (int64, bool) {
	if configured <= 0 {
		return avail, false
	}
	if configured > avail {
		return avail, true
	}
	return configured, false
}

// fitElastic is the admission arithmetic, split out to be testable without a
// provisioner. The baseline must fit; the elastic cap is then bounded by the
// leftover so on-demand growth cannot OOM the machine.
func fitElastic(available int64, plan *runPlan, configuredMaxLive int) (int, error) {
	usable := available - hostMemoryReserve
	need := compulsoryMemory(plan)
	if need > usable {
		return 0, fmt.Errorf("this agent needs %s for its always-on cages but the machine has %s usable (%s total): lower its RESOURCES caps, BAN or drop USES sub-agents (if it is yours), or use a machine with more memory",
			HumanBytes(need), HumanBytes(usable), HumanBytes(available))
	}
	rep := maxElasticMem(plan)
	if rep <= 0 {
		return configuredMaxLive, nil
	}
	if memCap := int((usable - need) / rep); memCap < configuredMaxLive {
		return memCap, nil
	}
	return configuredMaxLive, nil
}

func HumanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMiB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
