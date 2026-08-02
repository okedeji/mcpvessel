package mcpregistry

import (
	"fmt"

	"github.com/okedeji/mcpvessel/internal/bundle"
)

// Server is the MCP Registry's server.json record, spelled exactly as the
// v0.1 wire schema; a schema bump changes tags here and nowhere else.
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

// Package is one way to obtain a server: an npm/PyPI/OCI artifact plus how
// to launch it.
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

// Transport is how a launched package speaks MCP. Only stdio can be
// wrapped as an mcpvessel agent.
type Transport struct {
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
}

// Remote is a hosted MCP endpoint. An entry with only remotes has no code
// to run in a cage.
type Remote struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// KeyValueInput is a declared input (env var or argument), mapped onto the
// generated Vesselfile's ENV and SECRETS.
type KeyValueInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	IsRequired  bool   `json:"isRequired,omitempty"`
	IsSecret    bool   `json:"isSecret,omitempty"`
	Value       string `json:"value,omitempty"`
	Default     string `json:"default,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

// Argument is a launch argument; only the fields import echoes through are
// modeled.
type Argument struct {
	Type        string `json:"type,omitempty"`
	Name        string `json:"name,omitempty"`
	Value       string `json:"value,omitempty"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

// serverList is the GET /servers envelope; unmodeled fields are ignored.
type serverList struct {
	Servers  []serverEnvelope `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
		Count      int    `json:"count"`
	} `json:"metadata"`
}

// serverEnvelope wraps each list entry: the server.json under "server",
// registry-assigned metadata under a sibling "_meta". The publisher's own
// _meta, where the eval stamp rides, lives inside the server object.
type serverEnvelope struct {
	Server Server         `json:"server"`
	Meta   map[string]any `json:"_meta,omitempty"`
}

// isLatest reports whether the registry marks this entry as the name's current
// version. False when the flag is missing, so a registry that does not stamp it
// falls back to the caller's own ordering rather than claiming an old entry is
// current.
func (e serverEnvelope) isLatest() bool {
	official, ok := e.Meta[officialMetaKey].(map[string]any)
	if !ok {
		return false
	}
	latest, _ := official[officialIsLatestKey].(bool)
	return latest
}

const (
	// officialMetaKey is the registry's own metadata namespace on a list entry,
	// where it stamps which version of a name is current.
	officialMetaKey     = "io.modelcontextprotocol.registry/official"
	officialIsLatestKey = "isLatest"

	// publisherMetaKey is the registry's one free-form publisher slot, and the
	// only _meta namespace a publisher may set. Everything mcpvessel records
	// rides inside it: a top-level _meta key outside the registry's own
	// namespaces is rejected on publish (422 "unexpected property").
	publisherMetaKey = "io.modelcontextprotocol.registry/publisher-provided"

	// Keys mcpvessel writes inside the publisher-provided object.
	providedToolKey         = "tool"          // the tool that published (always "mcpvessel")
	providedEvalsKey        = "evals"         // the stamped eval signal, when present
	providedImportedFromKey = "imported_from" // the wrapped server's canonical identity

	// maxDescription is the registry's server.json description ceiling.
	maxDescription = 100

	// schemaURI is required in $schema on publish; the registry 422s without it.
	schemaURI = "https://static.modelcontextprotocol.io/schemas/2025-09-29/server.schema.json"
)

// ServerJSONFromManifest builds the registry record for a public agent:
// name is the reverse-DNS namespace the caller proved it owns, ociRef and
// version point at the pushed bundle, any eval stamp rides under _meta.
func ServerJSONFromManifest(m bundle.Manifest, name, ociRef, version string) *Server {
	// Everything rides inside the one free-form publisher slot; sibling
	// top-level _meta keys are rejected on publish (422 "unexpected property").
	provided := map[string]any{providedToolKey: "mcpvessel"}
	if m.Evals != nil {
		provided[providedEvalsKey] = m.Evals
	}
	if from := m.Vesselfile.Meta["imported_from"]; from != "" {
		provided[providedImportedFromKey] = from
	}
	meta := map[string]any{publisherMetaKey: provided}
	return &Server{
		Schema:      schemaURI,
		Name:        name,
		Description: description(m, name),
		Version:     version,
		Packages: []Package{{
			RegistryType: "oci",
			// The registry wants an OCI package's version in the identifier
			// (ghcr.io/owner/name:tag), not the version field.
			Identifier: ociRef + ":" + version,
			Transport:  Transport{Type: "stdio"},
		}},
		Meta: meta,
	}
}

// OCIReference returns the coordinates of the entry's oci package, or
// ok=false when it has none (a server mcpvessel did not publish).
func (s *Server) OCIReference() (ref, version string, ok bool) {
	for _, p := range s.Packages {
		if p.RegistryType == "oci" {
			return p.Identifier, p.Version, true
		}
	}
	return "", "", false
}

// EvalSummary renders the stamped eval signal as a compact "47/50 j0.83",
// or "" when none. It reads the generic wire map, which survives the
// registry round-trip where the typed publish shape does not.
func (s *Server) EvalSummary() string {
	raw, ok := s.publisherProvided()[providedEvalsKey].(map[string]any)
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

// ImportedFrom returns the canonical identity of the server this entry
// wraps, or "" when the entry is not an mcpvessel wrapper.
func (s *Server) ImportedFrom() string {
	from, _ := s.publisherProvided()[providedImportedFromKey].(string)
	return from
}

// publisherProvided returns mcpvessel's free-form _meta slot, or nil. Indexing
// the nil result is safe, so callers read keys straight off it.
func (s *Server) publisherProvided() map[string]any {
	p, _ := s.Meta[publisherMetaKey].(map[string]any)
	return p
}

// description resolves the required 1-100 char description from META, with
// a name-derived fallback so publish never fails the length rule.
func description(m bundle.Manifest, name string) string {
	d := m.Vesselfile.Meta["description"]
	if d == "" {
		d = "mcpvessel agent " + name
	}
	if len(d) > maxDescription {
		d = d[:maxDescription]
	}
	return d
}
