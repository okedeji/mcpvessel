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
	"strings"

	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/store"
)

// Bundle resolves arg to a local bundle path. An existing file is used as-is,
// so a hand-moved bundle or a -o output still works. A bare sha256 hash, full
// or a prefix, resolves an unnamed build from the store. Anything else is a
// reference, resolved store-first: a locally built bundle is found without a
// round-trip, and only a reference the store lacks is pulled (cache-first).
// display is the label a human view shows for the bundle: the file path, the
// hash, or the resolved ref.
func Bundle(ctx context.Context, arg string) (path, display string, err error) {
	if info, statErr := os.Stat(arg); statErr == nil && !info.IsDir() {
		return arg, arg, nil
	}

	if isContentHash(arg) {
		st, err := store.New()
		if err != nil {
			return "", "", err
		}
		p, ok, err := st.FindByHash(arg)
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", fmt.Errorf("%s: no bundle with that content hash in the store", arg)
		}
		return p, arg, nil
	}

	ref, err := reference.Parse(arg)
	if err != nil {
		return "", "", fmt.Errorf("%q is neither a local bundle nor a registry reference: %w", arg, err)
	}
	if ref.Tag == "" && ref.Digest == "" {
		return "", "", fmt.Errorf("%s: a version tag or digest is required", arg)
	}

	st, err := store.New()
	if err != nil {
		return "", "", err
	}
	if p, ok, err := st.Get(ref); err != nil {
		return "", "", err
	} else if ok {
		return p, ref.OCIRef(), nil
	}

	client, err := registry.New()
	if err != nil {
		return "", "", err
	}
	p, _, err := client.Pull(ctx, ref)
	if err != nil {
		return "", "", err
	}
	return p, ref.OCIRef(), nil
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
