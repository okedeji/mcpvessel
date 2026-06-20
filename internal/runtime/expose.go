package runtime

import (
	"context"
	"fmt"
	"sort"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/reference"
)

// Exposure is serve's access-control list for one served run: the agents an
// external caller may reach, keyed by their run-unique tree key, and for each
// the tools it may invoke. The served root is always present; sub-agents are
// reachable only when a USES PUBLIC edge (or an operator override) puts them
// there. serve computes it once from the resolved tree and consults it per
// external call, so a denied or private agent never reaches a held run.
type Exposure struct {
	Agents map[string]ExposedAgent
}

// ExposedAgent is one externally reachable agent: its tree key, the
// digest-pinned reference it was pulled by (the zero value for the served
// root, which carries no pull reference), the bundle serve boots it from as its
// own run, the URL segment a caller addresses it by, and the tools that caller
// may invoke, the agent's MAIN plus its EXPOSE'd tools and never a private one.
type ExposedAgent struct {
	Key     string
	Ref     reference.Reference
	Bundle  string
	Address string
	Tools   []string
}

// ExposureOverrides is the operator's serve-time exposure flags. Each entry is
// a reference matched to tree nodes by registry and repository, the same way a
// BAN matches, so a version-less @org/name catches every pin of that agent.
type ExposureOverrides struct {
	Expose   []string
	NoExpose []string
}

// computeExposure resolves which agents serve exposes for a run and which of
// each agent's tools an external caller may invoke.
//
// The set starts at the served root plus any agent the operator force-exposed,
// then grows along USES PUBLIC edges: an agent a parent marked PUBLIC is
// callable alongside that parent, and the walk repeats so exposure propagates
// down a chain that stays public. The operator's --no-expose is applied last
// and wins, hiding exactly the agents it names even when a PUBLIC edge reached
// them. The root is never hidden; serving a run you cannot reach is pointless.
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

// exposureRootKey names the served root in the tree exposure resolves. The run
// id does not matter here (exposure pulls no containers up), so a fixed key
// keeps ResolveExposure pure of run identity.
const exposureRootKey = "root"

// ResolveExposure resolves the served bundle's USES tree and returns the agents
// serve exposes: the root plus every USES PUBLIC sub-agent the overrides leave
// reachable. Each carries the bundle serve boots it from, a URL-safe address an
// external caller reaches it at, and its public tool names. rootAddress names
// the served root, which has no pull reference of its own; sub-agents take their
// address from their repository. Two agents resolving to one address is an error
// rather than a silent last-writer-wins over the front door's routing table.
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

// refMatcher is the registry-and-repository half of a reference, the part an
// exposure override matches on so a pin's tag or digest does not have to be
// named.
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

// publicTools is an agent's externally callable tool names: its MAIN tool plus
// its EXPOSE'd tools. It mirrors the cage-entry gate in cmd_call so the front
// door and a direct call agree on what is public.
func publicTools(m *bundle.Manifest) []string {
	if m == nil {
		return nil
	}
	tools := make([]string, 0, len(m.Agentfile.Expose)+1)
	if m.Agentfile.Main != "" {
		tools = append(tools, m.Agentfile.Main)
	}
	return append(tools, m.Agentfile.Expose...)
}
