package runtime

import "github.com/okedeji/agentcage/internal/config"

// Role-based default resource caps. A cage is never started uncapped: a
// hostile or runaway one must not starve CPU, OOM its siblings, or fork-bomb
// the host. Gateways are static binaries that barely allocate, so they get a
// tight cap. The agent default is sized for a typical MCP or reasoning server
// (the ones we run sit well under 100 MiB) with room to spare, not for the
// heavy outlier; an agent that drives a browser or crunches data is the case an
// operator raises the cap for. A smaller default also lets more cages fit a
// given machine, which a generous one quietly starved. These hold when the
// operator configures nothing; operator overrides land in a later step.
var (
	defaultAgentCap   = config.Cap{CPUs: "2", Mem: "512m", Pids: 1024}
	defaultGatewayCap = config.Cap{CPUs: "1", Mem: "128m", Pids: 128}
)

// withCap returns spec with the nerdctl resource fields set from c. Every
// container the runtime builds passes through this, so none is uncapped.
func (spec ContainerSpec) withCap(c config.Cap) ContainerSpec {
	spec.Memory = c.Mem
	spec.CPUs = c.CPUs
	spec.Pids = c.Pids
	return spec
}
