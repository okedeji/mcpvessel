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
		return Result{Path: p, Display: ref.OCIRef(), Name: path.Base(ref.Repository)}, nil
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
