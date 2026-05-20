package fleet

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/okedeji/agentcage/internal/config"
)

// InitPool populates a PoolManager with hosts from config. If no hosts are
// configured (local/single-machine mode), a single local host is added with
// auto-detected system resources.
func InitPool(pool *PoolManager, hosts []config.HostConfig, validationRes, discoveryRes, exploitationRes CageResources) error {
	if len(hosts) > 0 {
		for _, hc := range hosts {
			// Config hosts are always pinned. Static infrastructure
			// managed outside agentcage; only webhook-provisioned
			// hosts can be auto-drained and terminated.
			h := Host{
				ID:            hostIDFromAddress(hc.Address),
				Pool:          PoolActive,
				State:         HostReady,
				Pinned:        true,
				VCPUsTotal:    hc.VCPUs,
				MemoryMBTotal: hc.MemoryMB,
			}
			if hc.CageSlots > 0 {
				h.CageSlotsTotal = hc.CageSlots
			} else {
				h.CageSlotsTotal = CalculateMixedSlots(h, validationRes, discoveryRes, exploitationRes)
			}
			if h.CageSlotsTotal <= 0 {
				return fmt.Errorf("host %s has zero cage slots (vcpus=%d, mem=%dMB)", h.ID, h.VCPUsTotal, h.MemoryMBTotal)
			}
			if err := pool.AddHost(h); err != nil {
				return fmt.Errorf("adding host %s: %w", h.ID, err)
			}
		}
		return nil
	}

	// Local mode: single host with system resources. Reserve 2 vCPUs
	// and 2GB for the control plane (orchestrator, Temporal, Postgres,
	// NATS); the rest is available for cages.
	cpus := int32(runtime.NumCPU())
	memMB := int32(16384) // 16GB default for local dev

	reservedCPUs := int32(2)
	reservedMemMB := int32(2048)
	availCPUs := cpus - reservedCPUs
	availMemMB := memMB - reservedMemMB
	if availCPUs < 1 {
		availCPUs = 1
	}
	if availMemMB < 1024 {
		availMemMB = 1024
	}

	h := Host{
		ID:            "local",
		Pool:          PoolActive,
		State:         HostReady,
		Pinned:        true,
		VCPUsTotal:    availCPUs,
		MemoryMBTotal: availMemMB,
	}
	h.CageSlotsTotal = CalculateMixedSlots(h, validationRes, discoveryRes, exploitationRes)
	if h.CageSlotsTotal <= 0 {
		h.CageSlotsTotal = 1
	}
	return pool.AddHost(h)
}

func hostIDFromAddress(addr string) string {
	id := strings.ReplaceAll(addr, ".", "-")
	id = strings.ReplaceAll(id, ":", "-")
	id = strings.ReplaceAll(id, "/", "")
	if id == "" {
		return "host-unknown"
	}
	return id
}
