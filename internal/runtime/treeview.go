package runtime

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/okedeji/agentcage/internal/bundle"
)

// PrintTree writes an indented view of every agent a root bundle would run,
// so an author can see the full surface and decide what to BAN. Same
// digest-locked walk a run resolves the tree with.
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
	if g := nodeGlance(root); g != "" {
		_, _ = fmt.Fprintf(w, "  %s\n", g)
	}
	renderTreeChildren(tree, tree.Root, "", map[string]bool{tree.Root: true}, w)

	_, _ = fmt.Fprintf(w, "\nBaseline memory (always-on): ~%s\n", HumanBytes(treeBaselineMemory(tree)))
	_, _ = fmt.Fprintln(w, "  Elastic sub-agents activate on demand, bounded by cages.max_live.")

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
// sub-agent. onPath makes a malformed graph's back-edge print "(cycle)"
// instead of recursing forever.
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
		if g := nodeGlance(tree.Nodes[e.Sub].Manifest); g != "" {
			_, _ = fmt.Fprintf(w, "%s%s\n", childPrefix, g)
		}
		onPath[e.Sub] = true
		renderTreeChildren(tree, e.Sub, childPrefix, onPath, w)
		delete(onPath, e.Sub)
	}
}

// nodeGlance is the one-line operational summary of an agent for the tree
// view; full per-agent detail stays in inspect.
func nodeGlance(m *bundle.Manifest) string {
	if m == nil {
		return ""
	}
	af := m.Agentfile
	var parts []string
	if af.Model != "" {
		parts = append(parts, "model="+af.Model)
	}
	if af.Budget != 0 {
		parts = append(parts, "budget=$"+formatMicrosUSD(af.Budget))
	}
	if r := af.Resources; r != nil {
		parts = append(parts, "resources="+resourcesGlance(r))
	}
	if af.Egress != "" {
		parts = append(parts, "egress="+af.Egress)
	}
	if names := envGlanceNames(af.Env); len(names) > 0 {
		parts = append(parts, "env=["+strings.Join(names, ",")+"]")
	}
	if len(af.Secrets) > 0 {
		parts = append(parts, "secrets=["+strings.Join(af.Secrets, ",")+"]")
	}
	if n := exposedToolCount(m.Tools); n > 0 {
		parts = append(parts, fmt.Sprintf("tools=%d", n))
	}
	return strings.Join(parts, "  ")
}

// formatMicrosUSD renders integer micro-USD as a dollar string, trimming
// trailing zeros past two decimals.
func formatMicrosUSD(micros int64) string {
	s := fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000)
	s = strings.TrimRight(s, "0")
	switch dot := strings.IndexByte(s, '.'); {
	case dot == len(s)-1:
		return s + "00"
	case len(s)-dot-1 < 2:
		return s + "0"
	default:
		return s
	}
}

func resourcesGlance(r *bundle.ResourcesSpec) string {
	var parts []string
	if r.CPUs != "" {
		parts = append(parts, "cpu="+r.CPUs)
	}
	if r.Mem != "" {
		parts = append(parts, "mem="+r.Mem)
	}
	if r.Pids != 0 {
		parts = append(parts, fmt.Sprintf("pids=%d", r.Pids))
	}
	return strings.Join(parts, ",")
}

// envGlanceNames lists declared ENV keys, marking a required input (empty
// default) with a trailing *.
func envGlanceNames(envs map[string]string) []string {
	keys := sortedStringKeys(envs)
	for i, k := range keys {
		if envs[k] == "" {
			keys[i] = k + "*"
		}
	}
	return keys
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func exposedToolCount(tools []bundle.Tool) int {
	n := 0
	for _, t := range tools {
		if t.Visibility == bundle.VisibilityMain || t.Visibility == bundle.VisibilityPublic {
			n++
		}
	}
	return n
}

// nodeLabel identifies a sub-agent as @org/name plus a short digest, the form
// an author writes back into a BAN.
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
