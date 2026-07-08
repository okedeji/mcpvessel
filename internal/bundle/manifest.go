// Package bundle reads an agent's source directory plus its Agentfile
// and packages them into a .agent file: a gzip-tarball of the source
// tree alongside a manifest.json that describes it.
package bundle

import "time"

// Manifest is the JSON document at the root of every .agent file: the parsed
// Agentfile, a hash pinning the source tree, and build metadata.
type Manifest struct {
	SpecVersion string        `json:"spec_version"`
	Agentfile   AgentfileSpec `json:"agentfile"`
	Tools       []Tool        `json:"tools,omitempty"`
	Evals       *Evals        `json:"evals,omitempty"`
	FilesHash   string        `json:"files_hash"`
	BuiltAt     time.Time     `json:"built_at"`
	BuiltWith   string        `json:"built_with"`
}

// Evals is the eval status carried in a manifest. Declared is set at build
// time from the EVAL directive; the run fields are stamped after a full-suite
// run. They are pointers so a suite that never ran (all nil) reads apart from
// one that ran and scored zero.
type Evals struct {
	Declared   bool       `json:"declared"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	Passed     *int       `json:"passed,omitempty"`
	Failed     *int       `json:"failed,omitempty"`
	JudgeScore *float64   `json:"judge_score,omitempty"`
}

// Tool is one entry in the agent's tool catalog. The catalog lists every
// tool, private included, so the full capability surface is auditable;
// listing a private tool does not make it callable, visibility stays the
// access gate.
type Tool struct {
	Name        string         `json:"name"`
	Visibility  Visibility     `json:"visibility"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
}

// Visibility is a tool's role. The MCP routing layer gates on it: main and
// public are reachable from outside the cage, private is not.
type Visibility string

const (
	VisibilityMain    Visibility = "main"
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

// AgentfileSpec is the wire form of a parsed Agentfile, decoupled from the
// parser's in-memory types so the manifest schema can evolve independently.
type AgentfileSpec struct {
	From       string            `json:"from"`
	Entrypoint string            `json:"entrypoint"`
	Run        []string          `json:"run,omitempty"`
	Model      string            `json:"model,omitempty"` // "provider/name"
	Main       string            `json:"main,omitempty"`  // name of the tool that runs on `agentcage run`; omitted for tool collections
	Expose     []string          `json:"expose,omitempty"`
	Uses       []UseSpec         `json:"uses,omitempty"`
	Ban        []BanSpec         `json:"ban,omitempty"`    // agents (or their tools) forbidden anywhere in the subtree
	Budget     int64             `json:"budget,omitempty"` // USD cost cap per run in micro-USD
	Resources  *ResourcesSpec    `json:"resources,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Secrets    []string          `json:"secrets,omitempty"`
	Egress     string            `json:"egress,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
	Eval       string            `json:"eval,omitempty"`
}

// ResourcesSpec is the wire form of the advisory RESOURCES directive: the
// author's hint, not the enforced cap, which the operator sets.
type ResourcesSpec struct {
	CPUs string `json:"cpus,omitempty"`
	Mem  string `json:"mem,omitempty"`
	Pids int    `json:"pids,omitempty"`
}

// UseSpec is one entry in AgentfileSpec.Uses. An empty Deny accepts every
// EXPOSE'd tool of the sub-agent. Digest is the sha256 the tag resolved to at
// build time, the lockfile: the daemon pulls by digest, so a dependency
// re-pushed under the same tag does not change what this bundle runs against.
// A pre-resolver bundle carries no digest and the daemon falls back to the tag.
type UseSpec struct {
	Ref     string   `json:"ref"`
	Version string   `json:"version"`
	Digest  string   `json:"digest,omitempty"`
	Public  bool     `json:"public,omitempty"`
	Deny    []string `json:"deny,omitempty"`
}

// BanSpec is one BAN entry: an agent the root forbids anywhere in its
// subtree. Empty Tools bans the whole agent; non-empty bans those tools on
// every edge that reaches it, however deep.
type BanSpec struct {
	Ref   string   `json:"ref"`
	Tools []string `json:"tools,omitempty"`
}

// specVersion is the manifest schema version. Bump on incompatible change.
const specVersion = "0.1"
