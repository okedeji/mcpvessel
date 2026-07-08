package wrap

import (
	"fmt"
	"strings"
)

// ParseCoordinate reads a direct package coordinate:
//
//	npm:@modelcontextprotocol/server-filesystem@1.0
//	pypi:mcp-server-fetch==0.2
//	oci:ghcr.io/acme/mcp-slack:1.2
//
// ok=false when s carries no recognized registry prefix. A direct
// coordinate declares no inputs; the operator edits ENV/SECRETS into the
// generated Agentfile as needed.
func ParseCoordinate(s string) (Source, bool, error) {
	registry, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Source{}, false, nil
	}
	switch registry {
	case NPM, PyPI, OCI:
	default:
		return Source{}, false, nil
	}
	if rest == "" {
		return Source{}, false, fmt.Errorf("%s: missing the package after %q", s, registry+":")
	}

	id, version := splitVersion(registry, rest)
	if id == "" {
		return Source{}, false, fmt.Errorf("%s: missing the package name", s)
	}
	return Source{Registry: registry, Identifier: id, Version: version}, true, nil
}

// splitVersion peels the version off per registry: npm's trailing @version
// (past a leading @scope), pip's ==, an OCI :tag or @digest.
func splitVersion(registry, rest string) (id, version string) {
	switch registry {
	case NPM:
		if at := strings.LastIndex(rest, "@"); at > 0 {
			return rest[:at], rest[at+1:]
		}
		return rest, ""
	case PyPI:
		if i := strings.Index(rest, "=="); i >= 0 {
			return rest[:i], rest[i+2:]
		}
		return rest, ""
	default: // OCI
		if at := strings.LastIndex(rest, "@"); at >= 0 && strings.HasPrefix(rest[at+1:], "sha256:") {
			return rest[:at], rest[at+1:]
		}
		lastSlash := strings.LastIndex(rest, "/")
		segment := rest[lastSlash+1:]
		if c := strings.LastIndex(segment, ":"); c >= 0 {
			return rest[:lastSlash+1] + segment[:c], segment[c+1:]
		}
		return rest, ""
	}
}
