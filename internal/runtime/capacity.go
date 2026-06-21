package runtime

import (
	"fmt"

	"github.com/okedeji/agentcage/internal/config"
)

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

// admitMemory reads the machine's memory and decides whether the run fits: it
// errors when the compulsory baseline does not, and otherwise returns the elastic
// cap that the leftover memory allows, clamped to the operator's configured cap.
// It runs before any container starts.
func admitMemory(p Provisioner, plan *runPlan, configuredMaxLive int) (int, error) {
	avail, err := p.AvailableMemory()
	if err != nil {
		return 0, fmt.Errorf("reading machine memory: %w", err)
	}
	return fitElastic(avail, plan, configuredMaxLive)
}

// fitElastic is the memory arithmetic admitMemory wraps, split out so it is
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
