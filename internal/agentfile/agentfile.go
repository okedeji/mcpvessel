// Package agentfile parses and validates the Agentfile, the declarative
// manifest at the root of every agent's source tree.
package agentfile

import (
	"io"
	"os"
)

// Agentfile is the parsed and validated manifest for one agent.
//
// Required fields (Base, Entrypoint) are populated for every successfully
// parsed Agentfile. Optional fields use Go zero values: empty string for
// unset scalar directives, nil for slice-shaped directives that did not
// appear, an empty map for Env and Meta.
type Agentfile struct {
	From       string            // FROM: OCI image reference
	Entrypoint string            // ENTRYPOINT: command line that starts the MCP server
	Run        []string          // RUN: ordered build-time commands
	Model      *Model            // MODEL: nil when unset
	Main       string            // MAIN: name of the agent's reasoning-entry tool; empty for tool collections
	Expose     []string          // EXPOSE: tool names that are publicly callable from outside the cage
	Uses       []Use             // USES: registry sub-agent dependencies
	Ban        []Ban             // BAN: agents (or their tools) forbidden anywhere in this agent's subtree
	Budget     int64             // BUDGET: advisory USD cost cap per run in micro-USD; the operator's --budget is the enforced cap. 0 when unset
	Resources  *Resources        // RESOURCES: advisory cpu/mem/pids hint; nil when unset
	Env        map[string]string // ENV: author-supplied environment variables, or value-less operator-required inputs
	Secrets    []string          // SECRETS: secret keys to inject at runtime
	Egress     string            // EGRESS: egress policy ("deny-default" or "allow:domain,domain")
	Meta       map[string]string // META: registry discovery metadata
	Eval       string            // EVAL: path to the eval suite
}

// Use is one USES dependency: a sub-agent the parent depends on.
//
// Public mirrors the USES PUBLIC modifier: true exposes the sub-agent
// alongside the parent's external surface, false leaves it encapsulated
// as an internal dependency.
//
// Deny lists tool names from the sub-agent this parent does not want
// called. An empty Deny means the parent accepts every tool the sub-agent
// EXPOSEs. The parser only records the list; the routing layer is what
// rejects a denied call, once sub-agent routing exists.
type Use struct {
	Ref     string // canonical "@org/name", without the tag
	Version string // tag, never "latest"
	Public  bool
	Deny    []string // tool names denied; nil means "everything they EXPOSE"
}

// Ban is one BAN directive: an agent the root forbids anywhere in its
// subtree. It is the subtree-wide, inherited counterpart to USES DENY. An
// ONLY clause sets Tools to narrow the ban to specific tool names; an empty
// Tools bans the whole agent, so it does not run and no edge reaches it. A
// tool-level ban leaves the agent running but rejects those tools on every
// edge that reaches it, however deep.
type Ban struct {
	Ref   string   // canonical "@org/name", without a version
	Tools []string // tool names from the ONLY clause; nil means the whole agent
}

// Model is the parsed MODEL directive. It is advisory: the LLM gateway
// resolves the provider against the operator's configured endpoints and
// falls back when it is not configured, so any provider name is valid and
// the parser does not check it against a fixed list.
type Model struct {
	Provider string // e.g. "openai", "anthropic", "openrouter"
	Name     string // e.g. "claude-opus-4-8", "gpt-5.5"
}

// Resources is the parsed RESOURCES directive: an advisory hint of what
// the agent needs. The operator sets the concrete enforced cap, so these
// numbers are surfaced to the operator but never applied directly.
type Resources struct {
	CPUs string // nerdctl --cpus value, e.g. "2", "0.5"; empty when unset
	Mem  string // nerdctl --memory value, e.g. "2g", "512m"; empty when unset
	Pids int    // nerdctl --pids-limit value; 0 when unset
}

// Parse reads an Agentfile from r and returns the validated result.
// Errors name the offending line number when possible.
func Parse(r io.Reader) (*Agentfile, error) {
	return parse(r)
}

// ParseFile is the convenience wrapper around Parse for a path on disk.
func ParseFile(path string) (*Agentfile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}
