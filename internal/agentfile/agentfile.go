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
	Base       string            // BASE: OCI image reference
	Entrypoint string            // ENTRYPOINT: command line that starts the MCP server
	Build      []string          // BUILD: ordered build-time commands
	Model      *Model            // MODEL: nil when unset
	Access     []Capability      // ACCESS: built-in capabilities the agent uses
	Uses       []Use             // USES: registry sub-agent dependencies
	Budget     int               // BUDGET: max LLM tokens per run, 0 when unset
	Env        map[string]string // ENV: author-supplied environment variables
	Secrets    []string          // SECRETS: secret keys to inject at runtime
	Network    string            // NETWORK: egress policy ("deny-default" or "allow:domain,domain")
	Meta       map[string]string // META: registry discovery metadata
	Eval       string            // EVAL: path to the eval suite
}

// Use is one USES dependency: a sub-agent the parent depends on.
//
// Public mirrors the USES PUBLIC modifier: true exposes the sub-agent
// alongside the parent's external surface, false leaves it encapsulated
// as an internal dependency.
type Use struct {
	Ref     string // canonical "@org/name", without the tag
	Version string // tag, never "latest"
	Public  bool
}

// Model is the parsed MODEL directive.
type Model struct {
	Provider ModelProvider // "openai", "anthropic"
	Name     string        // e.g. "claude-opus-4-8", "gpt-5.5"
}

// ModelProvider names the LLM provider the runtime routes to. Parser
// only emits the constants below.
type ModelProvider string

const (
	ProviderOpenAI    ModelProvider = "openai"
	ProviderAnthropic ModelProvider = "anthropic"
)

// Capability names a built-in runtime feature an ACCESS directive grants.
// Parser only emits the constants below.
type Capability string

const (
	CapShell           Capability = "shell"
	CapHeadlessBrowser Capability = "headless-browser"
	CapFilesystem      Capability = "filesystem"
	CapNetwork         Capability = "network"
	CapClock           Capability = "clock"
)

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
