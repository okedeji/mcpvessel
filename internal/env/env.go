// Package env is the single source of the AGENTCAGE_* environment variable
// names agentcage reads and injects, so the same string is never spelled
// out in two places. Agents and the Python SDK mirror these names on their
// own side; this package is the Go owner of that contract, and a typo here
// is caught at compile time instead of silently failing at runtime.
package env

import "strings"

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

// DefaultGatewayPort is where the gateway listens inside the run network
// and the port the injected AGENTCAGE_USES_<NAME>_URL values point at. The
// gateway command and the orchestrator both default to it so the listen
// side and the call side cannot drift.
const DefaultGatewayPort = "9000"

// Sub-agent routing variables the runtime injects into each agent in a
// USES tree.
const (
	// ServeHTTP, when set to a bind address, tells an agent to serve MCP
	// over streamable-HTTP instead of stdio. The runtime sets it on every
	// sub-agent so the gateway can reach it; the root parent leaves it
	// unset and speaks stdio to the host.
	ServeHTTP = Prefix + "SERVE_HTTP"
)

// UsesURL is the variable name carrying a sub-agent's gateway URL for the
// caller that USES it. NAME is the USES local name uppercased with dashes
// turned to underscores. The agent side (the SDK or a raw MCP client)
// derives the same name from the same ref, so this is the Go anchor of a
// cross-language contract: change the rule here and the agent side stops
// finding its sub-agents.
func UsesURL(name string) string {
	return Prefix + "USES_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_URL"
}
