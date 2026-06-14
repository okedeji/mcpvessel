package runtime

import "github.com/okedeji/agentcage/internal/config"

// Role-based default resource caps. A cage is never started uncapped: a
// hostile or runaway one must not starve CPU, OOM its siblings, or fork-bomb
// the host. Gateways are static binaries that barely allocate, so they get a
// tight cap; agent cages run untrusted code that may shell out or drive a
// browser, so they get headroom but stay bounded. These hold when the
// operator configures nothing; operator overrides land in a later step.
var (
	defaultAgentCap   = config.Cap{CPUs: "2", Mem: "1g", Pids: 1024}
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
