package fleet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCalculateSlots(t *testing.T) {
	tests := []struct {
		name     string
		host     Host
		res      CageResources
		expected int32
	}{
		{
			name:     "discovery on large host is memory limited",
			host:     Host{VCPUsTotal: 96, MemoryMBTotal: 196608},
			res:      CageResources{VCPUs: 2, MemoryMB: 4096},
			expected: 48,
		},
		{
			name:     "validation on large host is CPU limited",
			host:     Host{VCPUsTotal: 96, MemoryMBTotal: 196608},
			res:      CageResources{VCPUs: 1, MemoryMB: 1024},
			expected: 96,
		},
		{
			name:     "small host with discovery cages",
			host:     Host{VCPUsTotal: 4, MemoryMBTotal: 8192},
			res:      CageResources{VCPUs: 2, MemoryMB: 4096},
			expected: 2,
		},
		{
			name:     "zero vCPUs returns zero",
			host:     Host{VCPUsTotal: 96, MemoryMBTotal: 196608},
			res:      CageResources{VCPUs: 0, MemoryMB: 4096},
			expected: 0,
		},
		{
			name:     "zero memory returns zero",
			host:     Host{VCPUsTotal: 96, MemoryMBTotal: 196608},
			res:      CageResources{VCPUs: 2, MemoryMB: 0},
			expected: 0,
		},
		{
			name:     "negative resources returns zero",
			host:     Host{VCPUsTotal: 96, MemoryMBTotal: 196608},
			res:      CageResources{VCPUs: -1, MemoryMB: 4096},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateSlots(tt.host, tt.res)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCalculateMixedSlots(t *testing.T) {
	host := Host{VCPUsTotal: 96, MemoryMBTotal: 196608}
	validationRes := CageResources{VCPUs: 1, MemoryMB: 1024}
	discoveryRes := CageResources{VCPUs: 2, MemoryMB: 4096}
	exploitationRes := CageResources{VCPUs: 2, MemoryMB: 4096}

	got := CalculateMixedSlots(host, validationRes, discoveryRes, exploitationRes)

	// validation: 96 slots * 0.60 = 57.6
	// discovery: 48 slots * 0.25 = 12.0
	// exploitation: 48 slots * 0.15 = 7.2
	// total = 76.8 → 77 (rounded)
	assert.Equal(t, int32(77), got)
}
