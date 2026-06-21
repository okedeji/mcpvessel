package runtime

import (
	"fmt"
	"io"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
)

// usableMemory applies the operator's machine.memory_gib to the machine's real
// memory and warns when the setting asks for more than the machine has. Both the
// tree and single-container boot paths read it before admitting a run.
func usableMemory(p Provisioner, machineMemCap int64, stderr io.Writer) (int64, error) {
	avail, err := p.AvailableMemory()
	if err != nil {
		return 0, fmt.Errorf("reading machine memory: %w", err)
	}
	usable, overRequest := effectiveAvailable(avail, machineMemCap)
	if overRequest {
		_, _ = fmt.Fprintf(stderr, "note: machine.memory_gib requests %s but the machine has %s; recreate the VM to apply on macOS, or lower it below host RAM on Linux. Using %s\n",
			humanBytes(machineMemCap), humanBytes(avail), humanBytes(avail))
	}
	return usable, nil
}

// soloBaselineMemory is the always-on memory a single-container run needs: the
// agent's cage, plus the LLM gateway if it reasons and the egress proxy if it
// declares allow:. There is no MCP gateway, since a lone agent has no USES tree.
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

// CageMemoryBytes is the memory one cage gets: its manifest's RESOURCES hint, or
// the runtime default when it states none. inspect shows it per agent; the tree
// view sums it across the compulsory set for a run's baseline.
func CageMemoryBytes(m *bundle.Manifest) int64 {
	if m != nil && m.Agentfile.Resources != nil {
		if b := (config.Cap{Mem: m.Agentfile.Resources.Mem}).MemBytes(); b > 0 {
			return b
		}
	}
	return defaultAgentCap.MemBytes()
}

// treeBaselineMemory is the always-on memory a run of this tree needs: the root
// cage, the gateway singletons the tree requires, and the egress sub-agent cages
// (compulsory, since the egress proxy keys them by a stable IP). Elastic
// sub-agents are not counted: they activate on demand, bounded by the live-cage
// cap. It uses the authors' RESOURCES hints (or the default), an advisory
// estimate the operator's config may adjust at run time.
func treeBaselineMemory(tree *runTree) int64 {
	total := CageMemoryBytes(tree.Nodes[tree.Root].Manifest)
	gw := defaultGatewayCap.MemBytes()
	if len(tree.Edges) > 0 {
		total += gw // the MCP gateway is present in any USES tree
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

// hostMemoryReserve is held back from the machine's total when deciding whether a
// run fits: the VM's own kernel, containerd, and buildkitd are not cages but they
// occupy memory, so the cages get the rest. A flat gibibyte is a deliberate
// over-estimate; better to refuse a run that would have just fit than to OOM the
// host mid-run.
const hostMemoryReserve = 1 << 30

// compulsoryMemory sums the memory a run's always-on cages need: the root, the
// gateway singletons the tree requires, and every kept-warm sub-agent. This is
// the baseline that must fit the machine before the run boots; the elastic cages
// grow into whatever is left. Banned agents never boot and are already absent
// from plan.Agents, so a BAN shrinks this; a tool-level DENY does not, since the
// agent still runs.
func compulsoryMemory(plan *runPlan) int64 {
	total := plan.RootCap.MemBytes()

	gw := defaultGatewayCap.MemBytes()
	total += gw // the MCP gateway is always present in a USES tree
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

// maxElasticMem is the largest memory cap among the run's elastic cages, the
// worst-case size one activation can take. Dividing the leftover memory by it
// gives the most elastic cages that fit without risking OOM.
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

// effectiveAvailable applies the operator's machine.memory_gib to the machine's
// real memory. A setting below it caps capacity, reserving the rest of the host
// for other things. A setting above it asks for more than the machine has, so it
// is ignored (the real amount is used) and flagged true, which lets the caller
// tell the operator to recreate the VM (macOS) or lower the setting (Linux).
// Zero means no setting: use the real memory as-is.
func effectiveAvailable(avail, configured int64) (int64, bool) {
	if configured <= 0 {
		return avail, false
	}
	if configured > avail {
		return avail, true
	}
	return configured, false
}

// fitElastic is the memory arithmetic the admission wraps, split out so it is
// testable without a provisioner. The baseline must fit the usable memory; the
// elastic cap is then bounded by the leftover so on-demand growth cannot OOM the
// machine even when the configured cap is higher.
func fitElastic(available int64, plan *runPlan, configuredMaxLive int) (int, error) {
	usable := available - hostMemoryReserve
	need := compulsoryMemory(plan)
	if need > usable {
		return 0, fmt.Errorf("this agent needs %s for its always-on cages but the machine has %s usable (%s total): lower its RESOURCES caps, BAN or drop USES sub-agents (if it is yours), or use a machine with more memory",
			humanBytes(need), humanBytes(usable), humanBytes(available))
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

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMiB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
