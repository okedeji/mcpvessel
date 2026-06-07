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
	FilesHash   string        `json:"files_hash"`
	BuiltAt     time.Time     `json:"built_at"`
	BuiltWith   string        `json:"built_with"`
}

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
	Budget     int               `json:"budget,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Secrets    []string          `json:"secrets,omitempty"`
	Network    string            `json:"network,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
	Eval       string            `json:"eval,omitempty"`
}

// UseSpec is one entry in AgentfileSpec.Uses. Public mirrors the
// USES PUBLIC modifier in the Agentfile.
type UseSpec struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
	Public  bool   `json:"public"`
}

// specVersion is the on-disk version of the manifest schema. Bump when
// the schema changes incompatibly.
const specVersion = "0.1"
