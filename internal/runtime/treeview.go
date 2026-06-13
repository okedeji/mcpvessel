package runtime

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/okedeji/agentcage/internal/bundle"
)

// PrintTree walks the transitive USES tree from a local root bundle and
// writes an indented view of every agent that would run, so an author can
// see the full surface and decide what to BAN. Each dependency is pulled by
// its locked digest, the same walk a run resolves the tree with.
func PrintTree(ctx context.Context, rootBundlePath, rootDisplay string, w io.Writer) error {
	root, err := bundle.ReadManifest(rootBundlePath)
	if err != nil {
		return err
	}
	tree, err := resolveRunTree(ctx, "root", rootBundlePath, root)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(w, rootDisplay)
	renderTreeChildren(tree, tree.Root, "", map[string]bool{tree.Root: true}, w)

	if len(root.Agentfile.Ban) > 0 {
		_, _ = fmt.Fprintln(w, "\nBans (declared here, applied across the whole subtree):")
		for _, b := range root.Agentfile.Ban {
			line := "  " + b.Ref
			if len(b.Tools) > 0 {
				line += " ONLY " + strings.Join(b.Tools, ",")
			}
			_, _ = fmt.Fprintln(w, line)
		}
	}
	return nil
}

// renderTreeChildren prints the USES edges out of caller, recursing into each
// sub-agent. onPath carries the keys from the root to here so a back-edge in
// a malformed graph prints "(cycle)" instead of recursing forever; a
// well-formed tree is a DAG and never hits it.
func renderTreeChildren(tree *runTree, caller, prefix string, onPath map[string]bool, w io.Writer) {
	var edges []usesEdge
	for _, e := range tree.Edges {
		if e.Caller == caller {
			edges = append(edges, e)
		}
	}
	for i, e := range edges {
		branch, childPrefix := "├─ ", prefix+"│  "
		if i == len(edges)-1 {
			branch, childPrefix = "└─ ", prefix+"   "
		}
		label := e.Alias + "  " + nodeLabel(tree.Nodes[e.Sub])
		if len(e.Deny) > 0 {
			label += "  DENY " + strings.Join(e.Deny, ",")
		}
		if onPath[e.Sub] {
			_, _ = fmt.Fprintf(w, "%s%s%s  (cycle)\n", prefix, branch, label)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s%s%s\n", prefix, branch, label)
		onPath[e.Sub] = true
		renderTreeChildren(tree, e.Sub, childPrefix, onPath, w)
		delete(onPath, e.Sub)
	}
}

// nodeLabel identifies a sub-agent: its @org/name and a short digest, the
// form an author writes back into a BAN.
func nodeLabel(node *agentNode) string {
	label := "@" + node.Ref.Repository
	short := strings.TrimPrefix(node.Ref.Digest, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	if short != "" {
		label += "  sha256:" + short
	}
	return label
}
