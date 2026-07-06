// Package locate turns a bundle path or registry reference into a local .agent
// file, the step run, call, inspect, and tree share before they can read or run
// an agent. An existing file is taken as-is; a reference resolves against the
// local store first and is pulled from the registry only when the store does
// not hold it.
package locate

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/store"
)

// Result is a resolved bundle: its local Path, the Display label a human view
// shows (a file path, a hash, or the full ref), and the short Name a run id
// derives from (the file or repo basename, or a short hash). Name keeps a run
// reading as "echo-..." rather than the store's content-hash filename.
type Result struct {
	Path    string
	Display string
	Name    string
}

// Bundle resolves arg to a local bundle. An existing file is used as-is, so a
// hand-moved bundle or a -o output still works. A bare sha256 hash, full or a
// prefix, resolves an unnamed build from the store. Anything else is a
// reference, resolved store-first: a locally built bundle is found without a
// round-trip, and only a reference the store lacks is pulled (cache-first).
func Bundle(ctx context.Context, arg string) (Result, error) {
	if info, statErr := os.Stat(arg); statErr == nil && !info.IsDir() {
		return Result{Path: arg, Display: arg, Name: fileName(arg)}, nil
	}

	if isContentHash(arg) {
		st, err := store.New()
		if err != nil {
			return Result{}, err
		}
		p, ok, err := st.FindByHash(arg)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, fmt.Errorf("%s: no bundle with that content hash in the store", arg)
		}
		return Result{Path: p, Display: arg, Name: shortHash(arg)}, nil
	}

	if name, ok := RegistryName(arg); ok {
		resolved, err := resolveReverseDNS(ctx, name)
		if err != nil {
			return Result{}, err
		}
		arg = resolved
	}

	ref, err := reference.Parse(arg)
	if err != nil {
		return Result{}, fmt.Errorf("%q is neither a local bundle nor a registry reference: %w", arg, err)
	}
	if ref.Tag == "" && ref.Digest == "" {
		return Result{}, fmt.Errorf("%s: a version tag or digest is required", arg)
	}

	st, err := store.New()
	if err != nil {
		return Result{}, err
	}
	if p, ok, err := st.Get(ref); err != nil {
		return Result{}, err
	} else if ok {
		return Result{Path: p, Display: ref.Display(), Name: path.Base(ref.Repository)}, nil
	}

	client, err := registry.New()
	if err != nil {
		return Result{}, err
	}
	p, _, err := client.Pull(ctx, ref)
	if err != nil {
		return Result{}, err
	}
	return Result{Path: p, Display: ref.OCIRef(), Name: path.Base(ref.Repository)}, nil
}

// RegistryName reports whether arg is an MCP Registry name (io.github.user/server)
// rather than an OCI reference, and returns it unchanged when so. The grammar is
// the discriminator: a registry name has exactly one slash, a dotted namespace
// on the left, and no tag or digest, which no valid agentcage OCI ref matches
// (those carry a host/org/name path of two slashes, or an @ shorthand). So the
// check never steals an input the reference parser would otherwise accept. It is
// exported so inspect can route a registry name to the registry view.
func RegistryName(arg string) (string, bool) {
	if strings.HasPrefix(arg, "@") || strings.Contains(arg, "@") {
		return "", false
	}
	left, right, ok := strings.Cut(arg, "/")
	if !ok || right == "" || strings.Contains(right, "/") || strings.Contains(right, ":") {
		return "", false
	}
	if !isReverseDNSNamespace(left) {
		return "", false
	}
	return arg, true
}

// isReverseDNSNamespace reports whether s is a reverse-DNS domain: two or more
// dot-separated alphanumeric labels. It is what separates a registry name's
// namespace from a relative path like "./x" or a bare word, so the caller does
// not mistake a file for a server. Uppercase is allowed because the registry
// preserves the case of a GitHub username, e.g. io.github.Digital-Defiance.
func isReverseDNSNamespace(s string) bool {
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" {
			return false
		}
		for _, c := range l {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// resolveReverseDNS asks the MCP Registry what OCI artifact a name points at and
// returns the ref the rest of Bundle resolves against. An entry with no OCI
// package is a server agentcage cannot pull, named as such rather than failing
// later with an opaque reference error.
func resolveReverseDNS(ctx context.Context, name string) (string, error) {
	server, err := mcpregistry.New().Resolve(ctx, name)
	if err != nil {
		return "", err
	}
	ref, version, ok := server.OCIReference()
	if !ok {
		return "", fmt.Errorf("%s: MCP Registry entry has no OCI artifact to pull", name)
	}
	if version != "" {
		return ref + ":" + version, nil
	}
	return ref, nil
}

// fileName is a bundle file's name without its .agent extension.
func fileName(p string) string {
	return strings.TrimSuffix(filepath.Base(p), ".agent")
}

// shortHash is the first 12 hex chars of a sha256 content-hash arg, the same
// width the runtime uses for run-id suffixes.
func shortHash(arg string) string {
	h := strings.TrimPrefix(arg, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// isContentHash reports whether arg is a bare sha256 hash, the form build
// prints for an unnamed bundle. A digest-pinned reference carries the same
// sha256 but behind a name@, so this only catches the standalone hash. The
// minimum length keeps a typo like "sha256:" from scanning the whole store.
func isContentHash(arg string) bool {
	rest, ok := strings.CutPrefix(arg, "sha256:")
	if !ok || len(rest) < 6 {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
