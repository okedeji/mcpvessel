// Package reference parses the agent references operators type on the
// command line and the registry references the manifest records, and
// resolves both to the OCI coordinates the registry layer pulls and
// pushes against.
//
// Two surface forms resolve to the same place:
//
//	@okedeji/researcher:0.1              agentcage-native shorthand
//	ghcr.io/okedeji/researcher:0.1       a fully-qualified OCI reference
//
// The shorthand has no host, so it maps to the default registry (ghcr.io,
// overridable via AGENTCAGE_REGISTRY). The fully-qualified form names its
// host and passes through untouched. This is the one place that knows the
// mapping; everything downstream works in OCI coordinates.
//
// Reverse-DNS MCP Registry names (io.github.<user>/...) are not special
// here. Recognizing one and resolving it to the OCI artifact it points at
// means querying the MCP Registry, which is the registry client's job; it
// hands back a fully-qualified ref that this package then parses. A caller
// that passes a reverse-DNS name straight in gets it read mechanically as
// a host of that name, which is the caller's bug to route, not ours to
// guess at.
package reference

import (
	"fmt"
	"os"
	"strings"

	"github.com/okedeji/agentcage/internal/env"
)

// fallbackRegistry is where host-less references resolve unless
// AGENTCAGE_REGISTRY overrides it. GHCR hosts OCI artifacts and offers
// immutable tags, which the digest lockfile depends on.
const fallbackRegistry = "ghcr.io"

// Reference is a parsed agent reference resolved to OCI coordinates.
//
// Tag and Digest are both optional and at most one is usually set: a
// reference pulled by tag carries Tag, a reference locked in a manifest
// carries Digest. A reference with neither is legal at this layer; the
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

// splitTagDigest peels the optional :tag or @digest off the end of a
// reference body, leaving the bare name. A digest wins over a tag when
// both somehow appear. The tag colon is found inside the final path
// segment so a host:port colon earlier in the string is left alone.
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

// resolveHost decides which OCI host a name belongs to and what the
// repository path under it is. Shorthand (@org/name) targets the default
// registry; anything whose first path segment looks like a host (carries
// a dot or a port) is taken at face value.
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

// defaultRegistry is where host-less references land. AGENTCAGE_REGISTRY
// overrides it for operators running against a private host.
func defaultRegistry() string {
	if v := strings.TrimSpace(os.Getenv(env.Registry)); v != "" {
		return v
	}
	return fallbackRegistry
}

// DefaultRegistry is the host shorthand references resolve to. The login
// command defaults to it when the operator names no registry.
func DefaultRegistry() string {
	return defaultRegistry()
}

// OCIRef is the canonical host/repository[:tag|@digest] string the
// registry layer pulls and pushes against. Digest wins over tag when
// both are set so a locked reference fetches exactly what it pinned.
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

// Display renders a reference for a human naming a local artifact: the
// @org/name shorthand when the registry is the default, and the full
// host/repository form for an explicit registry. It exists because OCIRef
// always spells out the default host, which reads as a registry association a
// local-only bundle does not have. Registry operations (push, pull, login) keep
// OCIRef, since there the real host is the point.
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
