package runtime

import (
	"context"
	"fmt"
	"sort"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/reference"
)

// Exposure is serve's access-control list for one served run: the agents an
// external caller may reach, keyed by tree key, and the tools each may invoke.
// The served root is always present; a sub-agent only via a USES PUBLIC edge
// or an operator override.
type Exposure struct {
	Agents map[string]ExposedAgent
}

// ExposedAgent is one externally reachable agent. Ref is zero for the served
// root, which carries no pull reference. Tools is the agent's MAIN plus its
// EXPOSE'd tools, never a private one.
type ExposedAgent struct {
	Key     string
	Ref     reference.Reference
	Bundle  string
	Address string
	Tools   []string
}

// ExposureOverrides is the operator's serve-time exposure flags. Entries match
// tree nodes by registry and repository, like a BAN, so a version-less
// @org/name catches every pin of that agent.
type ExposureOverrides struct {
	Expose   []string
	NoExpose []string
}

// computeExposure resolves which agents serve exposes. The set starts at the
// served root plus any force-exposed agent, then grows along USES PUBLIC
// edges; exposure propagates down a chain only while it stays public.
// --no-expose is applied last and wins. The root is never hidden.
func computeExposure(tree *runTree, ov ExposureOverrides) (Exposure, error) {
	expose, err := refMatchers(ov.Expose)
	if err != nil {
		return Exposure{}, err
	}
	hide, err := refMatchers(ov.NoExpose)
	if err != nil {
		return Exposure{}, err
	}

	exposed := map[string]bool{tree.Root: true}
	for key, node := range tree.Nodes {
		if key != tree.Root && matchesAny(node.Ref, expose) {
			exposed[key] = true
		}
	}

	for grew := true; grew; {
		grew = false
		for _, e := range tree.Edges {
			if e.Public && exposed[e.Caller] && !exposed[e.Sub] {
				exposed[e.Sub] = true
				grew = true
			}
		}
	}

	for key, node := range tree.Nodes {
		if key != tree.Root && matchesAny(node.Ref, hide) {
			delete(exposed, key)
		}
	}

	out := Exposure{Agents: make(map[string]ExposedAgent, len(exposed))}
	for key := range exposed {
		node := tree.Nodes[key]
		out.Agents[key] = ExposedAgent{
			Key:    key,
			Ref:    node.Ref,
			Bundle: node.Bundle,
			Tools:  publicTools(node.Manifest),
		}
	}
	return out, nil
}

// exposureRootKey names the served root. Exposure boots no containers, so a
// fixed key keeps ResolveExposure pure of run identity.
const exposureRootKey = "root"

// ResolveExposure resolves the served bundle's USES tree and returns the
// agents serve exposes, each with its bundle, URL-safe address, and public
// tool names. The root takes rootAddress; a sub-agent's address comes from its
// repository. Two agents resolving to one address is an error, not a silent
// last-writer-wins over the front door's routing table.
func ResolveExposure(ctx context.Context, bundlePath, rootAddress string, ov ExposureOverrides) ([]ExposedAgent, error) {
	manifest, err := bundle.ReadManifest(bundlePath)
	if err != nil {
		return nil, err
	}
	tree, err := resolveRunTree(ctx, exposureRootKey, bundlePath, manifest)
	if err != nil {
		return nil, err
	}
	exp, err := computeExposure(tree, ov)
	if err != nil {
		return nil, err
	}

	seen := map[string]string{}
	out := make([]ExposedAgent, 0, len(exp.Agents))
	for key, a := range exp.Agents {
		if key == tree.Root {
			a.Address = sanitizeRef(rootAddress)
		} else {
			a.Address = sanitizeRef(a.Ref.Repository)
		}
		if other, dup := seen[a.Address]; dup {
			return nil, fmt.Errorf("exposed agents %s and %s both resolve to address %q; hide one with --no-expose", other, key, a.Address)
		}
		seen[a.Address] = key
		out = append(out, a)
	}

	sort.Slice(out, func(i, j int) bool {
		if (out[i].Key == tree.Root) != (out[j].Key == tree.Root) {
			return out[i].Key == tree.Root
		}
		return out[i].Address < out[j].Address
	})
	return out, nil
}

// refMatcher is the registry-and-repository half of a reference; overrides
// match on it so a pin's tag or digest need not be named.
type refMatcher struct {
	registry   string
	repository string
}

func refMatchers(refs []string) ([]refMatcher, error) {
	out := make([]refMatcher, 0, len(refs))
	for _, r := range refs {
		ref, err := reference.Parse(r)
		if err != nil {
			return nil, fmt.Errorf("exposure override %q: %w", r, err)
		}
		out = append(out, refMatcher{ref.Registry, ref.Repository})
	}
	return out, nil
}

func matchesAny(ref reference.Reference, ms []refMatcher) bool {
	for _, m := range ms {
		if ref.Registry == m.registry && ref.Repository == m.repository {
			return true
		}
	}
	return false
}

// publicTools is an agent's externally callable tool names: MAIN plus its
// EXPOSE'd tools. The catalog is the authoritative visibility when present
// (it has EXPOSE * expanded, the raw directive does not); a declared-only
// bundle falls back to the raw names, where a bare "*" cannot be expanded
// and is dropped. Mirrors the cage-entry gate in cmd_call.
func publicTools(m *bundle.Manifest) []string {
	if m == nil {
		return nil
	}
	if len(m.Tools) > 0 {
		tools := make([]string, 0, len(m.Tools))
		for _, t := range m.Tools {
			if t.Visibility == bundle.VisibilityMain || t.Visibility == bundle.VisibilityPublic {
				tools = append(tools, t.Name)
			}
		}
		return tools
	}
	tools := make([]string, 0, len(m.Agentfile.Expose)+1)
	if m.Agentfile.Main != "" {
		tools = append(tools, m.Agentfile.Main)
	}
	for _, name := range m.Agentfile.Expose {
		if name == "*" {
			continue
		}
		tools = append(tools, name)
	}
	return tools
}
