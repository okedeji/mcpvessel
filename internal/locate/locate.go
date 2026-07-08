// Package locate turns a bundle path or registry reference into a local
// .agent file, the step run, call, inspect, and tree share. An existing
// file is taken as-is; a reference resolves store-first and is pulled only
// on a miss.
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

// Result is a resolved bundle: local Path, the Display label human views
// show, and the short Name run ids derive from. Name keeps a run reading as
// "echo-..." rather than the store's content-hash filename.
type Result struct {
	Path    string
	Display string
	Name    string
}

// Bundle resolves arg to a local bundle: an existing file as-is, a bare
// sha256 hash (full or prefix) from the store, anything else as a reference
// resolved store-first and pulled only when the store lacks it.
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

// RegistryName reports whether arg is an MCP Registry name
// (io.github.user/server). The grammar discriminates: exactly one slash, a
// dotted namespace, no tag or digest, which no valid agentcage OCI ref
// matches, so this never steals the reference parser's input.
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

// isReverseDNSNamespace matches two or more dot-separated alphanumeric
// labels. Uppercase is allowed: the registry preserves GitHub username
// case, e.g. io.github.Digital-Defiance.
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

// resolveReverseDNS resolves a name to the OCI ref its registry entry
// points at. An entry with no OCI package cannot be pulled and is named as
// such rather than failing later with an opaque reference error.
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

func fileName(p string) string {
	return strings.TrimSuffix(filepath.Base(p), ".agent")
}

// shortHash is the first 12 hex chars, the run-id suffix width.
func shortHash(arg string) string {
	h := strings.TrimPrefix(arg, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// isContentHash matches a bare sha256 hash, the form build prints for an
// unnamed bundle; a digest-pinned reference hides its sha256 behind name@,
// so only the standalone form matches. The minimum length keeps a bare
// "sha256:" typo from scanning the whole store.
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
