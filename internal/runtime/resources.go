package runtime

import "github.com/okedeji/agentcage/internal/config"

// Role-based default caps; a cage is never started uncapped. Gateways are
// static binaries that barely allocate. The agent default is sized for a
// typical MCP or reasoning server (well under 100 MiB), not the heavy outlier
// an operator raises the cap for; a smaller default also fits more cages per
// machine. Operator overrides land in a later step.
var (
	defaultAgentCap   = config.Cap{CPUs: "2", Mem: "512m", Pids: 1024}
	defaultGatewayCap = config.Cap{CPUs: "1", Mem: "128m", Pids: 128}
)

// withCap sets the nerdctl resource fields from c. Every container the
// runtime builds passes through here, so none is uncapped.
func (spec ContainerSpec) withCap(c config.Cap) ContainerSpec {
	spec.Memory = c.Mem
	spec.CPUs = c.CPUs
	spec.Pids = c.Pids
	return spec
}
