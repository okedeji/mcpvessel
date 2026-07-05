// Package bundle reads an agent's source directory plus its Agentfile
// and packages them into a .agent file: a gzip-tarball of the source
// tree alongside a manifest.json that describes it.
package bundle

import "time"

// Manifest is the JSON document stored at the root of every .agent file.
// It records the parsed Agentfile, a hash that pins the source tree, and
// a small amount of build metadata.
//
// Bundle consumers read the manifest to validate integrity (files_hash)
// and to decide how to run the agent (the embedded agentfile spec).
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
// run of `agentcage eval` or `agentcage push --with-evals`.
//
// The run fields are pointers so a bundle that declares an EVAL suite but has
// never run it (all nil) reads apart from one that ran and scored zero. A
// consumer sees "declared, never run" versus "0 passed" without guessing.
type Evals struct {
	Declared   bool       `json:"declared"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	Passed     *int       `json:"passed,omitempty"`
	Failed     *int       `json:"failed,omitempty"`
	JudgeScore *float64   `json:"judge_score,omitempty"`
}

// Tool is one entry in the agent's tool catalog. The catalog lists every
// tool the agent has (main, public, and private) so consumers can review
// the full capability surface before depending on the agent. Listing a
// private tool here does not make it callable from outside the cage;
// visibility stays the access gate.
//
// Today the catalog holds only the MAIN and EXPOSE tools, with name and
// visibility set. Build-time introspection enriches each entry with a
// description and schema and adds the private tools the agent serves.
type Tool struct {
	Name        string         `json:"name"`
	Visibility  Visibility     `json:"visibility"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
}

// Visibility distinguishes the three roles a tool can have in an agent.
// The MCP routing layer uses it to decide whether an external caller can
// reach a tool: main and public are reachable, private is not. The catalog
// still publishes private entries so reviewers can audit the full surface;
// the access gate stays closed regardless.
type Visibility string

const (
	VisibilityMain    Visibility = "main"
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

// AgentfileSpec is the wire-format representation of a parsed Agentfile.
// It is decoupled from the parser's in-memory types so that the manifest
// schema can evolve independently of how we choose to model directives
// in Go.
//
// Fields tagged omitempty are omitted from JSON when unset, keeping
// manifests for minimal agents concise.
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

// ResourcesSpec is the wire form of the advisory RESOURCES directive. The
// operator sets the concrete enforced cap; these are the author's hint.
type ResourcesSpec struct {
	CPUs string `json:"cpus,omitempty"`
	Mem  string `json:"mem,omitempty"`
	Pids int    `json:"pids,omitempty"`
}

// UseSpec is one entry in AgentfileSpec.Uses. Public mirrors the
// USES PUBLIC modifier; Deny carries the parent's exclusion list from
// the `USES @ref:ver DENY tool1,tool2` clause. An empty Deny means
// the parent accepts every EXPOSE'd tool of the sub-agent.
//
// Digest is the sha256 the sub-agent's tag resolved to at build time. It
// is the lockfile: the daemon pulls by digest, not tag, so a dependency
// re-pushed under the same tag does not change what this bundle runs
// against. A bundle built before the resolver carries no digest; omitempty
// keeps that manifest valid and the daemon falls back to the tag for it.
type UseSpec struct {
	Ref     string   `json:"ref"`
	Version string   `json:"version"`
	Digest  string   `json:"digest,omitempty"`
	Public  bool     `json:"public,omitempty"`
	Deny    []string `json:"deny,omitempty"`
}

// BanSpec is one entry in AgentfileSpec.Ban: an agent the root forbids
// anywhere in its subtree, from the `BAN @ref [ONLY tool1,tool2]` directive.
// An empty Tools bans the whole agent (it does not run and no edge reaches
// it); a non-empty Tools bans those tools on every edge that reaches the
// agent, however deep.
type BanSpec struct {
	Ref   string   `json:"ref"`
	Tools []string `json:"tools,omitempty"`
}

// specVersion is the on-disk version of the manifest schema. Bump when
// the schema changes incompatibly.
const specVersion = "0.1"
