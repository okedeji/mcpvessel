package runtime

import (
	"context"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
)

func agent(main string, expose []string, uses ...bundle.UseSpec) *bundle.Manifest {
	return &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Main: main, Expose: expose, Uses: uses}}
}

func pubUse(ref, version, digest string) bundle.UseSpec {
	u := use(ref, version, digest)
	u.Public = true
	return u
}

func mustTree(t *testing.T, root *bundle.Manifest, byDigest map[string]*bundle.Manifest) *runTree {
	t.Helper()
	tree, err := resolveTree(context.Background(), "root", "/root.agent", root, fakePuller(byDigest))
	if err != nil {
		t.Fatalf("resolveTree: %v", err)
	}
	return tree
}

func exposedTools(exp Exposure) map[string]bool {
	s := map[string]bool{}
	for _, a := range exp.Agents {
		for _, tool := range a.Tools {
			s[tool] = true
		}
	}
	return s
}

func TestComputeExposure_PublicExposedPrivateHidden(t *testing.T) {
	const (
		digScraper = "sha256:aaaaaaaaaaaa0000"
		digCred    = "sha256:bbbbbbbbbbbb0000"
	)
	root := agent("search", []string{"summarize"},
		pubUse("@org/web-scraper", "0.1", digScraper),
		use("@org/creddb", "0.1", digCred),
	)
	scraper := agent("fetch", nil)
	cred := agent("lookup", nil)

	tree := mustTree(t, root, map[string]*bundle.Manifest{digScraper: scraper, digCred: cred})
	exp, err := computeExposure(tree, ExposureOverrides{})
	if err != nil {
		t.Fatalf("computeExposure: %v", err)
	}

	if len(exp.Agents) != 2 {
		t.Fatalf("exposed agents = %d, want 2 (root, web-scraper)", len(exp.Agents))
	}
	tools := exposedTools(exp)
	for _, want := range []string{"search", "summarize", "fetch"} {
		if !tools[want] {
			t.Errorf("tool %q not exposed, want it reachable", want)
		}
	}
	if tools["lookup"] {
		t.Error("private sub-agent's tool lookup is exposed, want it hidden")
	}
}

func TestComputeExposure_PropagatesThroughPublicChain(t *testing.T) {
	const (
		digA = "sha256:aaaaaaaaaaaa0000"
		digB = "sha256:bbbbbbbbbbbb0000"
	)
	a := agent("ta", nil, pubUse("@org/b", "1.0", digB))
	b := agent("tb", nil)

	publicChain := agent("troot", nil, pubUse("@org/a", "1.0", digA))
	tree := mustTree(t, publicChain, map[string]*bundle.Manifest{digA: a, digB: b})
	exp, err := computeExposure(tree, ExposureOverrides{})
	if err != nil {
		t.Fatalf("computeExposure: %v", err)
	}
	if len(exp.Agents) != 3 {
		t.Fatalf("exposed = %d, want 3 (a public, b public through a)", len(exp.Agents))
	}

	// A private link anywhere breaks the chain: b is no longer reachable.
	aPrivateB := agent("ta", nil, use("@org/b", "1.0", digB))
	brokenRoot := agent("troot", nil, pubUse("@org/a", "1.0", digA))
	tree = mustTree(t, brokenRoot, map[string]*bundle.Manifest{digA: aPrivateB, digB: b})
	exp, err = computeExposure(tree, ExposureOverrides{})
	if err != nil {
		t.Fatalf("computeExposure: %v", err)
	}
	if len(exp.Agents) != 2 {
		t.Fatalf("exposed = %d, want 2 (root, a); b is behind a private edge", len(exp.Agents))
	}
	if exposedTools(exp)["tb"] {
		t.Error("b exposed through a private edge, want it hidden")
	}
}

func TestComputeExposure_Overrides(t *testing.T) {
	const (
		digScraper = "sha256:aaaaaaaaaaaa0000"
		digCred    = "sha256:bbbbbbbbbbbb0000"
	)
	root := agent("search", nil,
		pubUse("@org/web-scraper", "0.1", digScraper),
		use("@org/creddb", "0.1", digCred),
	)
	scraper := agent("fetch", nil)
	cred := agent("lookup", nil)
	byDigest := map[string]*bundle.Manifest{digScraper: scraper, digCred: cred}

	exposeCred := mustTree(t, root, byDigest)
	exp, err := computeExposure(exposeCred, ExposureOverrides{Expose: []string{"@org/creddb"}})
	if err != nil {
		t.Fatalf("computeExposure: %v", err)
	}
	if !exposedTools(exp)["lookup"] {
		t.Error("--expose @org/creddb did not expose the private sub-agent")
	}

	hideScraper := mustTree(t, root, byDigest)
	exp, err = computeExposure(hideScraper, ExposureOverrides{NoExpose: []string{"@org/web-scraper"}})
	if err != nil {
		t.Fatalf("computeExposure: %v", err)
	}
	if exposedTools(exp)["fetch"] {
		t.Error("--no-expose @org/web-scraper did not hide the public sub-agent")
	}
	if !exposedTools(exp)["search"] {
		t.Error("the served root was hidden, want it always reachable")
	}
}

func TestComputeExposure_BadOverrideRef(t *testing.T) {
	tree := mustTree(t, agent("search", nil), nil)
	if _, err := computeExposure(tree, ExposureOverrides{Expose: []string{"@org/x:"}}); err == nil {
		t.Fatal("computeExposure accepted a malformed override reference")
	}
}

func TestPublicTools(t *testing.T) {
	if got := publicTools(agent("main", []string{"a", "b"})); len(got) != 3 || got[0] != "main" {
		t.Errorf("publicTools = %v, want [main a b]", got)
	}
	if got := publicTools(agent("", []string{"only"})); len(got) != 1 || got[0] != "only" {
		t.Errorf("publicTools without MAIN = %v, want [only]", got)
	}
	if got := publicTools(nil); got != nil {
		t.Errorf("publicTools(nil) = %v, want nil", got)
	}
}
