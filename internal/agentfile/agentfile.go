// Package agentfile parses and validates the Agentfile, the declarative
// manifest at the root of every agent's source tree.
package agentfile

import (
	"io"
	"os"
)

// Agentfile is the parsed and validated manifest for one agent. From and
// Entrypoint are always set; optional directives use Go zero values, with
// Env and Meta as empty maps.
type Agentfile struct {
	From       string            // FROM: OCI image reference
	Entrypoint string            // ENTRYPOINT: command line that starts the MCP server
	Run        []string          // RUN: ordered build-time commands
	Model      *Model            // MODEL: nil when unset
	Main       string            // MAIN: name of the agent's reasoning-entry tool; empty for tool collections
	Expose     []string          // EXPOSE: tool names that are publicly callable from outside the cage
	Uses       []Use             // USES: registry sub-agent dependencies
	Ban        []Ban             // BAN: agents (or their tools) forbidden anywhere in this agent's subtree
	Budget     int64             // BUDGET: advisory per-run cap in micro-USD; the operator's --budget is enforced. 0 when unset
	Resources  *Resources        // RESOURCES: advisory cpu/mem/pids hint; nil when unset
	Env        map[string]string // ENV: author-supplied environment variables, or value-less operator-required inputs
	Secrets    []string          // SECRETS: secret keys to inject at runtime
	Egress     string            // EGRESS: egress policy ("deny-default" or "allow:domain,domain")
	Meta       map[string]string // META: registry discovery metadata
	Eval       string            // EVAL: path to the eval suite
}

// Use is one USES dependency. Public mirrors the PUBLIC modifier; Deny
// lists sub-agent tools the parent rejects, nil accepting everything the
// sub-agent EXPOSEs.
type Use struct {
	Ref     string // canonical "@org/name", without the tag
	Version string // tag, never "latest"
	Public  bool
	Deny    []string // tool names denied; nil means "everything they EXPOSE"
}

// Ban is one BAN directive: an agent the root forbids anywhere in its
// subtree, the inherited counterpart to USES DENY. Empty Tools bans the
// whole agent; a tool-level ban leaves it running but rejects those tools
// on every edge that reaches it.
type Ban struct {
	Ref   string   // canonical "@org/name", without a version
	Tools []string // tool names from the ONLY clause; nil means the whole agent
}

// Model is the parsed MODEL directive. Advisory: the LLM gateway resolves
// the provider at run time, so the parser accepts any provider name.
type Model struct {
	Provider string // e.g. "openai", "anthropic", "openrouter"
	Name     string // e.g. "claude-opus-4-8", "gpt-5.5"
}

// Resources is the parsed RESOURCES directive, an advisory hint. The
// operator sets the enforced cap; these numbers are never applied directly.
type Resources struct {
	CPUs string // nerdctl --cpus value, e.g. "2", "0.5"; empty when unset
	Mem  string // nerdctl --memory value, e.g. "2g", "512m"; empty when unset
	Pids int    // nerdctl --pids-limit value; 0 when unset
}

// Parse reads an Agentfile from r and returns the validated result.
func Parse(r io.Reader) (*Agentfile, error) {
	return parse(r)
}

// ParseFile is Parse for a path on disk.
func ParseFile(path string) (*Agentfile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}
