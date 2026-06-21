// Package env is the single source of the AGENTCAGE_* environment variable
// names agentcage reads and injects, so the same string is never spelled
// out in two places. Agents and the Python SDK mirror these names on their
// own side; this package is the Go owner of that contract, and a typo here
// is caught at compile time instead of silently failing at runtime.
package env

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HomeDir resolves the ~/.agentcage root where agentcage keeps its state,
// honoring AGENTCAGE_HOME so an operator can relocate all of it together.
// Callers join their own leaf onto it (the cache, the store, config.json, the
// daemon socket). AGENTCAGE_HOME, when set, is that root directly.
func HomeDir() (string, error) {
	if h := strings.TrimSpace(os.Getenv(Home)); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".agentcage"), nil
}

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

// MCP gateway variables the runtime injects into the MCP gateway container.
const (
	// MCPConfig is the JSON routing table the MCP gateway serves.
	MCPConfig = Prefix + "MCP_CONFIG"
	// MCPAddr is the MCP gateway's listen address.
	MCPAddr = Prefix + "MCP_ADDR"
)

// DefaultMCPGatewayPort is where the MCP gateway listens inside the run
// network and the port the injected AGENTCAGE_USES_<NAME>_URL values point
// at. The mcp-gateway command and the orchestrator both default to it so the
// listen side and the call side cannot drift.
const DefaultMCPGatewayPort = "9000"

// DefaultMCPControlPort is where the MCP gateway serves its activation control
// stream, next in the reserved 900x gateway series. Like the LLM control port it
// binds the container's loopback only, unreachable from the run network: the
// daemon reaches it by exec'ing the mcp-control bridge into the container, which
// is what lets a sandboxed gateway ask the host to boot an inactive sub-agent
// without ever holding an outbound channel of its own.
const DefaultMCPControlPort = "9004"

// LLM gateway variables. LLMURL is the only one a reasoning cage sees: an
// OpenAI-compatible endpoint it calls instead of a provider directly.
// LLMConfig and LLMAddr go into the LLM gateway container, which holds the
// provider keys and meters cost so the agent never sees a key.
const (
	// LLMURL is the per-agent OpenAI-compatible endpoint the runtime injects
	// into a reasoning cage. The agent side reads it; the runtime points it
	// at the LLM gateway with the agent's own path.
	LLMURL = Prefix + "LLM_URL"
	// LLMConfig is the JSON endpoint set, pricing, budget, and per-agent
	// models the LLM gateway serves.
	LLMConfig = Prefix + "LLM_CONFIG"
	// LLMAddr is the LLM gateway's listen address.
	LLMAddr = Prefix + "LLM_ADDR"
)

// DefaultLLMGatewayPort is where the LLM gateway listens, distinct from the
// MCP gateway's port so both can share the run network.
const DefaultLLMGatewayPort = "9001"

// DefaultLLMControlPort is where the LLM gateway serves its operator control
// surface (live budget changes, spend readout), next in the reserved 900x
// gateway series. Unlike the others it binds the container's loopback only, so
// agents on the run network cannot reach it; the daemon drives it through
// nerdctl exec, from inside the container's namespace.
const DefaultLLMControlPort = "9003"

// Egress proxy variables the runtime injects into the egress container,
// provisioned only when some agent declares EGRESS allow:. The proxy filters
// outbound connections by host and holds no secrets.
const (
	// EgressConfig is the JSON per-agent host allow-list the proxy enforces.
	EgressConfig = Prefix + "EGRESS_CONFIG"
	// EgressAddr is the egress proxy's listen address.
	EgressAddr = Prefix + "EGRESS_ADDR"
)

// DefaultEgressPort is where the egress proxy listens, distinct from the two
// gateways so all three share the run network without colliding.
const DefaultEgressPort = "9002"

// Sub-agent routing variables the runtime injects into each agent in a
// USES tree.
const (
	// ServeHTTP, when set to a bind address, tells an agent to serve MCP
	// over streamable-HTTP instead of stdio. The runtime sets it on every
	// sub-agent so the MCP gateway can reach it; the root parent leaves it
	// unset and speaks stdio to the host.
	ServeHTTP = Prefix + "SERVE_HTTP"
)

// Interaction tells the root agent whether its run can loop back to a caller.
// A one-shot run/call gets a single turn and is torn down after it, with no
// channel to ask a follow-up; a held or served run can be continued. An author
// keys a complete, best-effort answer off oneshot instead of hedging for a
// reply that will never come. It is advisory: the runtime sets it, the author
// reads it. The hard gate on mid-call elicitation is the MCP client capability,
// which a one-shot boot does not advertise.
const Interaction = Prefix + "INTERACTION"

// Interaction values. OneShot is a single run/call turn; Interactive is a held
// or served run a caller can continue.
const (
	InteractionOneShot     = "oneshot"
	InteractionInteractive = "interactive"
)

// UsesURL is the variable name carrying a sub-agent's MCP gateway URL for the
// caller that USES it. NAME is the USES local name uppercased with dashes
// turned to underscores. The agent side (the SDK or a raw MCP client)
// derives the same name from the same ref, so this is the Go anchor of a
// cross-language contract: change the rule here and the agent side stops
// finding its sub-agents.
func UsesURL(name string) string {
	return Prefix + "USES_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_URL"
}
