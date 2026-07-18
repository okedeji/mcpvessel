// Package env is the single source of the VESSEL_* environment variable
// names. Agents and the Python SDK mirror these names on their side; this
// package is the Go owner of that cross-language contract.
package env

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HomeDir resolves the ~/.mcpvessel state root. VESSEL_HOME, when set, is
// that root directly.
func HomeDir() (string, error) {
	if h := strings.TrimSpace(os.Getenv(Home)); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".mcpvessel"), nil
}

// Prefix is reserved for runtime-injected variables. The Vesselfile parser
// rejects author ENV keys carrying it, so a Vesselfile cannot shadow the
// runtime.
const Prefix = "VESSEL_"

// Config knobs the binary reads from its own environment.
const (
	// Registry overrides the default OCI host for shorthand references.
	Registry = Prefix + "REGISTRY"
	// Home overrides the ~/.mcpvessel root for cache and state.
	Home = Prefix + "HOME"
	// MCPRegistry overrides the official MCP Registry base URL.
	MCPRegistry = Prefix + "MCP_REGISTRY"
	// GitHubClientID is the OAuth app client id for 'login mcp-registry'.
	// No default: the command fails closed with instructions.
	GitHubClientID = Prefix + "GITHUB_CLIENT_ID"
	// RequireSignatures, when set truthy, refuses to pull any unsigned
	// bundle. Off by default while the ecosystem is unsigned.
	RequireSignatures = Prefix + "REQUIRE_SIGNATURES"
)

// MCP gateway variables the runtime injects into the MCP gateway container.
const (
	// MCPConfig is the JSON routing table the MCP gateway serves.
	MCPConfig = Prefix + "MCP_CONFIG"
	// MCPAddr is the MCP gateway's listen address.
	MCPAddr = Prefix + "MCP_ADDR"
)

// DefaultMCPGatewayPort is the MCP gateway's listen port on the run network,
// where the injected VESSEL_USES_<NAME>_URL values point. Both the
// mcp-gateway command and the orchestrator default to it.
const DefaultMCPGatewayPort = "9000"

// DefaultMCPControlPort serves the MCP gateway's activation control stream.
// Loopback only, unreachable from the run network; the daemon reaches it by
// exec'ing the mcp-control bridge into the container, so a sandboxed gateway
// can ask the host to boot a sub-agent without an outbound channel of its own.
const DefaultMCPControlPort = "9004"

// LLM gateway variables. LLMURL is the only one a reasoning cage sees; the
// gateway container holds the provider keys, so the agent never sees one.
const (
	// LLMURL is the per-agent OpenAI-compatible endpoint injected into a
	// reasoning cage, pointing at the LLM gateway with the agent's own path.
	LLMURL = Prefix + "LLM_URL"
	// LLMConfig is the JSON endpoint set, pricing, budget, and per-agent
	// models the LLM gateway serves.
	LLMConfig = Prefix + "LLM_CONFIG"
	// LLMAddr is the LLM gateway's listen address.
	LLMAddr = Prefix + "LLM_ADDR"
)

const DefaultLLMGatewayPort = "9001"

// DefaultLLMControlPort serves the LLM gateway's operator control surface
// (live budget changes, spend readout). Loopback only, so agents on the run
// network cannot reach it; the daemon drives it through nerdctl exec.
const DefaultLLMControlPort = "9003"

// Egress proxy variables, injected only when some agent declares EGRESS
// allow:.
const (
	// EgressConfig is the JSON per-agent host allow-list the proxy enforces.
	EgressConfig = Prefix + "EGRESS_CONFIG"
	// EgressAddr is the egress proxy's listen address.
	EgressAddr = Prefix + "EGRESS_ADDR"
)

const DefaultEgressPort = "9002"

// DefaultEgressControlPort serves the egress proxy's operator control surface
// (approve or reject a held host). Loopback only inside the proxy container;
// the daemon drives it via nerdctl exec, the same pattern as the LLM gateway.
const DefaultEgressControlPort = "9005"

const (
	// ServeHTTP, when set to a bind address, tells an agent to serve MCP over
	// streamable-HTTP instead of stdio. Set on every sub-agent; the root
	// parent leaves it unset and speaks stdio to the host.
	ServeHTTP = Prefix + "SERVE_HTTP"
)

// Interaction tells the root agent whether its run can loop back to a caller.
// Advisory only; the hard gate on mid-call elicitation is the MCP client
// capability, advertised only on an interactive boot, so a one-shot agent
// that tries to ask fails closed.
const Interaction = Prefix + "INTERACTION"

// Interaction values. OneShot is a single run/call turn; Interactive is a
// held or served run a caller can continue.
const (
	InteractionOneShot     = "oneshot"
	InteractionInteractive = "interactive"
)

// UsesURL is the variable name carrying a sub-agent's MCP gateway URL: the
// USES local name uppercased, dashes to underscores. The agent side derives
// the same name from the same ref; changing the rule here breaks that
// cross-language contract.
func UsesURL(name string) string {
	return Prefix + "USES_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_URL"
}
