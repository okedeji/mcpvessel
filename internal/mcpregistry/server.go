package mcpregistry

import (
	"fmt"

	"github.com/okedeji/agentcage/internal/bundle"
)

// Server is the MCP Registry's server.json record. Field names and casing
// follow the registry's v0.1 schema on the wire; this is the one place that
// spelling lives, so a schema bump changes tags here and nowhere else.
type Server struct {
	Schema      string         `json:"$schema,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Version     string         `json:"version"`
	Title       string         `json:"title,omitempty"`
	Repository  *Repository    `json:"repository,omitempty"`
	WebsiteURL  string         `json:"websiteUrl,omitempty"`
	Packages    []Package      `json:"packages,omitempty"`
	Remotes     []Remote       `json:"remotes,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

// Repository points at the source an entry was built from.
type Repository struct {
	URL    string `json:"url,omitempty"`
	Source string `json:"source,omitempty"`
}

// Package is one way to obtain a server: an npm/PyPI/OCI artifact plus how to
// launch it. import reads registryType and identifier to pick a wrapping
// strategy; publish writes a single oci package pointing at the agent's bundle.
type Package struct {
	RegistryType         string          `json:"registryType"`
	RegistryBaseURL      string          `json:"registryBaseUrl,omitempty"`
	Identifier           string          `json:"identifier"`
	Version              string          `json:"version,omitempty"`
	FileSHA256           string          `json:"fileSha256,omitempty"`
	RuntimeHint          string          `json:"runtimeHint,omitempty"`
	Transport            Transport       `json:"transport"`
	RuntimeArguments     []Argument      `json:"runtimeArguments,omitempty"`
	PackageArguments     []Argument      `json:"packageArguments,omitempty"`
	EnvironmentVariables []KeyValueInput `json:"environmentVariables,omitempty"`
}

// Transport is how a launched package speaks MCP. An agentcage agent is stdio;
// a package that only offers streamable-http or sse cannot be wrapped as one.
type Transport struct {
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
}

// Remote is a hosted MCP endpoint. An entry with only remotes and no packages
// is code agentcage cannot run in a cage, which is why import refuses it.
type Remote struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// KeyValueInput is a declared input (an env var or argument). import maps these
// onto the generated Agentfile's ENV and SECRETS so a wrapped server documents
// the same inputs it always did.
type KeyValueInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	IsRequired  bool   `json:"isRequired,omitempty"`
	IsSecret    bool   `json:"isSecret,omitempty"`
	Value       string `json:"value,omitempty"`
	Default     string `json:"default,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

// Argument is a launch argument. import does not synthesize arguments, so only
// the fields it may echo through are modeled.
type Argument struct {
	Type        string `json:"type,omitempty"`
	Name        string `json:"name,omitempty"`
	Value       string `json:"value,omitempty"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

// serverList is the GET /servers envelope. Only the fields agentcage reads are
// modeled; the registry may send more and the decoder ignores it.
type serverList struct {
	Servers  []serverEnvelope `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
		Count      int    `json:"count"`
	} `json:"metadata"`
}

// serverEnvelope is how the list endpoint wraps each entry: the server.json
// under "server", and registry-assigned metadata (status, timestamps) under a
// sibling "_meta". The publisher's own _meta, where an eval stamp rides, lives
// inside the server object, so unwrapping Server keeps that signal.
type serverEnvelope struct {
	Server Server         `json:"server"`
	Meta   map[string]any `json:"_meta,omitempty"`
}

const (
	// evalsMetaKey namespaces the eval signal agentcage stamps onto a public
	// entry, reverse-DNS per the registry's _meta extension rule so it never
	// collides with another publisher's fields.
	evalsMetaKey = "io.agentcage/evals"

	// publisherMetaKey is the registry's standard slot for who published and
	// with what tool.
	publisherMetaKey = "io.modelcontextprotocol.registry/publisher-provided"

	// maxDescription is the registry's server.json description ceiling.
	maxDescription = 100

	// schemaURI is the server.json schema the registry requires in the $schema
	// field on publish. It is a required field, so publish 422s without it.
	schemaURI = "https://static.modelcontextprotocol.io/schemas/2025-09-29/server.schema.json"
)

// ServerJSONFromManifest builds the registry record for a public agent: name is
// the reverse-DNS namespace the caller proved it owns, ociRef and version point
// at the pushed bundle, and any eval stamp rides along under _meta so registry
// browsers can show the quality signal.
func ServerJSONFromManifest(m bundle.Manifest, name, ociRef, version string) *Server {
	meta := map[string]any{
		publisherMetaKey: map[string]any{"tool": "agentcage"},
	}
	if m.Evals != nil {
		meta[evalsMetaKey] = m.Evals
	}
	return &Server{
		Schema:      schemaURI,
		Name:        name,
		Description: description(m, name),
		Version:     version,
		Packages: []Package{{
			RegistryType: "oci",
			// The registry requires an OCI package's version to ride in the
			// identifier (ghcr.io/owner/name:tag), not a separate version field.
			Identifier: ociRef + ":" + version,
			Transport:  Transport{Type: "stdio"},
		}},
		Meta: meta,
	}
}

// OCIReference returns the OCI coordinates an entry's oci package points at, so
// reverse-DNS resolution can hand a fully-qualified ref to the OCI client. It
// reports false when the entry has no oci package, which is the case for a
// server agentcage did not publish.
func (s *Server) OCIReference() (ref, version string, ok bool) {
	for _, p := range s.Packages {
		if p.RegistryType == "oci" {
			return p.Identifier, p.Version, true
		}
	}
	return "", "", false
}

// EvalSummary renders the eval signal an author stamped onto the entry, as a
// compact "47/50 j0.83" for a search row, or "" when none was stamped. It reads
// the generic map the wire decodes into, so it survives a round-trip through the
// registry that the typed publish side does not preserve.
func (s *Server) EvalSummary() string {
	raw, ok := s.Meta[evalsMetaKey].(map[string]any)
	if !ok {
		return ""
	}
	if declared, _ := raw["declared"].(bool); !declared {
		return ""
	}
	passed, hasP := raw["passed"].(float64)
	failed, hasF := raw["failed"].(float64)
	if !hasP && !hasF {
		return "declared"
	}
	out := fmt.Sprintf("%d/%d", int(passed), int(passed+failed))
	if js, ok := raw["judge_score"].(float64); ok {
		out += fmt.Sprintf(" j%.2f", js)
	}
	return out
}

// description resolves the entry's required 1-100 char description from the
// agent's META, falling back to a name-derived line so publish never fails the
// registry's length rule on a missing META.
func description(m bundle.Manifest, name string) string {
	d := m.Agentfile.Meta["description"]
	if d == "" {
		d = "agentcage agent " + name
	}
	if len(d) > maxDescription {
		d = d[:maxDescription]
	}
	return d
}
