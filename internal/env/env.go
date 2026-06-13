// Package env is the single source of the AGENTCAGE_* environment variable
// names agentcage reads and injects, so the same string is never spelled
// out in two places. Agents and the Python SDK mirror these names on their
// own side; this package is the Go owner of that contract, and a typo here
// is caught at compile time instead of silently failing at runtime.
package env

// Prefix is reserved for variables the runtime injects. The Agentfile
// parser rejects author ENV keys carrying it, so an Agentfile cannot shadow
// what the runtime sets.
const Prefix = "AGENTCAGE_"

// Config knobs the binary reads from its own environment.
const (
	// Registry overrides the default OCI host for shorthand references.
	Registry = Prefix + "REGISTRY"
	// Home overrides the ~/.agentcage root for cache and state.
	Home = Prefix + "HOME"
)

// Gateway variables the runtime injects into the gateway container.
const (
	// GatewayConfig is the JSON routing table the gateway serves.
	GatewayConfig = Prefix + "GATEWAY_CONFIG"
	// GatewayAddr is the gateway's listen address.
	GatewayAddr = Prefix + "GATEWAY_ADDR"
)
