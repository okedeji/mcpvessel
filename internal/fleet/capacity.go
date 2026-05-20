package fleet

import "math"

type CageResources struct {
	VCPUs    int32
	MemoryMB int32
}

func CalculateSlots(host Host, res CageResources) int32 {
	if res.VCPUs <= 0 || res.MemoryMB <= 0 {
		return 0
	}
	cpuSlots := host.VCPUsTotal / res.VCPUs
	memSlots := host.MemoryMBTotal / res.MemoryMB
	if cpuSlots < memSlots {
		return cpuSlots
	}
	return memSlots
}

// CalculateMixedSlots estimates total cage slots for a typical workload mix.
// Uses a default ratio of 60% validation, 25% discovery, 15% exploitation.
func CalculateMixedSlots(host Host, validationRes, discoveryRes, exploitationRes CageResources) int32 {
	validationSlots := CalculateSlots(host, validationRes)
	discoverySlots := CalculateSlots(host, discoveryRes)
	exploitationSlots := CalculateSlots(host, exploitationRes)
	return int32(math.Round(float64(validationSlots)*0.60 + float64(discoverySlots)*0.25 + float64(exploitationSlots)*0.15))
}
