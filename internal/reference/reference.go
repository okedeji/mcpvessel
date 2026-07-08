// Package reference parses agent references into OCI coordinates. The
// @org/name shorthand maps to the default registry (ghcr.io, overridable
// via AGENTCAGE_REGISTRY); a fully-qualified host/org/name passes through.
// Reverse-DNS MCP Registry names are not recognized here; resolving one is
// the registry client's job.
package reference

import (
	"fmt"
	"os"
	"strings"

	"github.com/okedeji/agentcage/internal/env"
)

// fallbackRegistry is where host-less references resolve. GHCR offers
// immutable tags, which the digest lockfile depends on.
const fallbackRegistry = "ghcr.io"

// Reference is a parsed agent reference in OCI coordinates. At most one of
// Tag and Digest is usually set; neither is legal at this layer, and the
// operation that needs one rejects it with its own error.
type Reference struct {
	Original   string // exactly what the caller passed, for error messages
	Registry   string // OCI host, e.g. "ghcr.io"
	Repository string // path within the host, e.g. "okedeji/researcher"
	Tag        string // "0.1", empty when pinned by digest or unset
	Digest     string // "sha256:...", empty when referenced by tag
}

// Parse resolves one reference string to its OCI coordinates.
func Parse(s string) (Reference, error) {
	original := s
	s = strings.TrimSpace(s)
	if s == "" {
		return Reference{}, fmt.Errorf("empty reference")
	}

	shorthand := strings.HasPrefix(s, "@")
	body := strings.TrimPrefix(s, "@")

	name, tag, digest, err := splitTagDigest(body)
	if err != nil {
		return Reference{}, fmt.Errorf("reference %q: %w", original, err)
	}

	registry, repository, err := resolveHost(name, shorthand)
	if err != nil {
		return Reference{}, fmt.Errorf("reference %q: %w", original, err)
	}

	return Reference{
		Original:   original,
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
	}, nil
}

// splitTagDigest peels the optional :tag or @digest off the body. The tag
// colon is searched only in the final path segment so a host:port colon is
// left alone; a digest wins when both somehow appear.
func splitTagDigest(body string) (name, tag, digest string, err error) {
	if at := strings.LastIndex(body, "@"); at >= 0 {
		name, digest = body[:at], body[at+1:]
		if !strings.HasPrefix(digest, "sha256:") || len(digest) <= len("sha256:") {
			return "", "", "", fmt.Errorf("digest %q is not a sha256 digest", digest)
		}
		return name, "", digest, nil
	}

	lastSlash := strings.LastIndex(body, "/")
	segment := body[lastSlash+1:]
	if colon := strings.LastIndex(segment, ":"); colon >= 0 {
		tag = segment[colon+1:]
		if tag == "" {
			return "", "", "", fmt.Errorf("empty tag")
		}
		name = body[:lastSlash+1] + segment[:colon]
		return name, tag, "", nil
	}

	return body, "", "", nil
}

// resolveHost decides which OCI host a name belongs to. Shorthand targets
// the default registry; a first path segment that looks like a host (a dot
// or a port) is taken at face value; anything else is ambiguous.
func resolveHost(name string, shorthand bool) (registry, repository string, err error) {
	if shorthand {
		if !strings.Contains(name, "/") {
			return "", "", fmt.Errorf("shorthand reference must be @org/name, missing the org")
		}
		return defaultRegistry(), name, nil
	}

	first, rest, ok := strings.Cut(name, "/")
	if ok && (strings.Contains(first, ".") || strings.Contains(first, ":")) {
		return first, rest, nil
	}

	return "", "", fmt.Errorf("ambiguous reference: write @org/name for the default registry or host/org/name for an explicit one")
}

func defaultRegistry() string {
	if v := strings.TrimSpace(os.Getenv(env.Registry)); v != "" {
		return v
	}
	return fallbackRegistry
}

// DefaultRegistry is the host shorthand references resolve to.
func DefaultRegistry() string {
	return defaultRegistry()
}

// publicHosts can be pulled without a private credential, so a push to one
// is a candidate for MCP Registry publication. Anything else is private.
var publicHosts = map[string]bool{
	"docker.io": true,
	"ghcr.io":   true,
	"quay.io":   true,
}

// IsPublicHost reports whether host is a known public OCI registry.
func IsPublicHost(host string) bool {
	return publicHosts[host]
}

// ReverseDNSName derives the MCP Registry name a reference publishes
// under, io.github.<owner>/<name>. Only GHCR maps, because GitHub is the
// namespace agentcage proves ownership of at login; any other host, or a
// path deeper than owner/name, reports false rather than guessing.
func (r Reference) ReverseDNSName() (string, bool) {
	if r.Registry != "ghcr.io" {
		return "", false
	}
	owner, name, ok := strings.Cut(r.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return "io.github." + owner + "/" + name, true
}

// OCIRef is the canonical host/repository[:tag|@digest] string. Digest wins
// over tag so a locked reference fetches exactly what it pinned.
func (r Reference) OCIRef() string {
	base := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return base + "@" + r.Digest
	}
	if r.Tag != "" {
		return base + ":" + r.Tag
	}
	return base
}

// Display renders the @org/name shorthand when the registry is the
// default, the full form otherwise. OCIRef always spells out the host,
// which reads as a registry association a local-only bundle does not have;
// registry operations keep OCIRef, where the real host is the point.
func (r Reference) Display() string {
	if r.Registry != defaultRegistry() {
		return r.OCIRef()
	}
	base := "@" + r.Repository
	if r.Digest != "" {
		return base + "@" + r.Digest
	}
	if r.Tag != "" {
		return base + ":" + r.Tag
	}
	return base
}

// String returns the canonical OCI reference.
func (r Reference) String() string {
	return r.OCIRef()
}
