package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/reference"
)

// agentNode is one agent in a run's USES tree: a unique key within the run,
// the reference it was pulled by, the bundle pinned to its digest, and the
// parsed manifest. The root is the agent the operator ran (no pull
// reference); the rest are USES dependencies pulled by digest.
type agentNode struct {
	Key      string
	Ref      reference.Reference
	Bundle   string
	Manifest *bundle.Manifest
}

// usesEdge is one USES relationship: Caller calls Sub, knowing it by Alias
// (the USES local name the caller's AGENTCAGE_USES_<ALIAS>_URL carries),
// with Deny tools the MCP gateway blocks on this edge. Public marks a USES
// PUBLIC edge: serve exposes Sub to external callers alongside its caller.
type usesEdge struct {
	Caller string
	Sub    string
	Alias  string
	Deny   []string
	Public bool
}

// runTree is the resolved dependency tree for one run: every unique agent
// keyed for the run, and every USES edge between them.
type runTree struct {
	Root  string
	Nodes map[string]*agentNode
	Edges []usesEdge
}

// pullManifest fetches a USES dependency by its locked reference and returns
// where the bundle landed plus its manifest. The orchestrator passes a
// registry-backed implementation; tests pass an in-memory one.
type pullManifest func(ctx context.Context, ref reference.Reference) (bundlePath string, m *bundle.Manifest, err error)

// resolveTree walks the transitive USES graph from the root manifest,
// pulling each dependency by its locked digest, and returns every unique
// agent and edge. The build-time resolver already pinned each USES to a
// digest and rejected cycles, so the walk pulls deterministically. The
// seen-check makes it terminate even if a malformed bundle reintroduced a
// cycle: a node is walked once, its edges recorded every time.
//
// A USES without a digest is a bundle built before the resolver. The
// runtime pulls by digest, so that is an error here rather than a silent
// fall back to a mutable tag.
func resolveTree(ctx context.Context, rootKey, rootBundle string, root *bundle.Manifest, pull pullManifest) (*runTree, error) {
	tree := &runTree{
		Root:  rootKey,
		Nodes: map[string]*agentNode{rootKey: {Key: rootKey, Bundle: rootBundle, Manifest: root}},
	}

	var walk func(callerKey string, m *bundle.Manifest) error
	walk = func(callerKey string, m *bundle.Manifest) error {
		for _, u := range m.Agentfile.Uses {
			if u.Digest == "" {
				return fmt.Errorf("USES %s:%s has no locked digest; rebuild the bundle so the runtime can pull by digest", u.Ref, u.Version)
			}
			subKey := nodeKey(u.Ref, u.Digest)
			tree.Edges = append(tree.Edges, usesEdge{
				Caller: callerKey,
				Sub:    subKey,
				Alias:  usesAlias(u.Ref),
				Deny:   u.Deny,
				Public: u.Public,
			})
			if _, seen := tree.Nodes[subKey]; seen {
				continue
			}

			ref, err := referenceForUse(u)
			if err != nil {
				return err
			}
			path, sm, err := pull(ctx, ref)
			if err != nil {
				return fmt.Errorf("pulling USES %s:%s: %w", u.Ref, u.Version, err)
			}
			tree.Nodes[subKey] = &agentNode{Key: subKey, Ref: ref, Bundle: path, Manifest: sm}
			if err := walk(subKey, sm); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(rootKey, root); err != nil {
		return nil, err
	}
	return tree, nil
}

// referenceForUse pins a USES entry to its locked digest as an OCI
// reference the registry pulls by content, not by the mutable tag.
func referenceForUse(u bundle.UseSpec) (reference.Reference, error) {
	ref, err := reference.Parse(u.Ref + "@" + u.Digest)
	if err != nil {
		return reference.Reference{}, fmt.Errorf("USES %s@%s: %w", u.Ref, u.Digest, err)
	}
	return ref, nil
}

// usesAlias is the local name a parent calls a sub-agent by: the last path
// segment of the USES ref. It feeds AGENTCAGE_USES_<ALIAS>_URL, so the
// agent side derives the same name from the same ref.
func usesAlias(ref string) string {
	ref = strings.TrimPrefix(ref, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// nodeKey is the run-unique key for one agent: its alias plus a short
// digest, so the same sub-agent pulled by two parents dedupes to one node
// while two pins of the same name stay distinct. It doubles as a container
// name component, so it is run through sanitizeRef.
func nodeKey(ref, digest string) string {
	short := strings.TrimPrefix(digest, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	return sanitizeRef(usesAlias(ref)) + "-" + short
}
