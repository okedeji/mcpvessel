package runtime

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/reference"
)

func TestScopedSecrets_For(t *testing.T) {
	s := ScopedSecrets{
		"":       {"SHARED": "a", "OVERLAP": "broadcast"},
		"sentry": {"SENTRY_TOKEN": "b", "OVERLAP": "scoped"},
	}
	got := s.For("sentry")
	want := map[string]string{"SHARED": "a", "SENTRY_TOKEN": "b", "OVERLAP": "scoped"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("For(sentry) = %v, want %v", got, want)
	}
	// An unscoped agent sees only the broadcast pool; the scoped grant never
	// reaches it. This is the property the whole feature exists for.
	got = s.For("brave")
	want = map[string]string{"SHARED": "a", "OVERLAP": "broadcast"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("For(brave) = %v, want %v", got, want)
	}
	if ScopedSecrets(nil).For("x") != nil {
		t.Error("nil pool should resolve to nil")
	}
}

func TestScopedSecrets_FlattenAndBroadcast(t *testing.T) {
	s := ScopedSecrets{"": {"A": "1"}, "x": {"B": "2"}}
	if got := s.Flatten(); !reflect.DeepEqual(got, map[string]string{"A": "1", "B": "2"}) {
		t.Errorf("Flatten = %v", got)
	}
	if got := Broadcast(map[string]string{"A": "1"}); !reflect.DeepEqual(got.For("anyone"), map[string]string{"A": "1"}) {
		t.Errorf("Broadcast round-trip = %v", got)
	}
	if Broadcast(nil) != nil {
		t.Error("Broadcast(nil) should be nil")
	}
}

// treeWith builds a minimal run tree: a root plus one node per (alias,
// secrets) pair, enough for the warning shapes.
func treeWith(rootSecrets []string, subs map[string][]string) *runTree {
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root": {Key: "root", Manifest: &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Secrets: rootSecrets}}},
		},
	}
	for alias, secrets := range subs {
		tree.Nodes[alias] = &agentNode{
			Key:      alias,
			Ref:      reference.Reference{Repository: "me/" + alias},
			Manifest: &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Secrets: secrets}},
		}
	}
	return tree
}

func TestWarnSecretShapes(t *testing.T) {
	tree := treeWith(nil, map[string][]string{
		"sentry-tools": {"API_TOKEN"},
		"brave-tools":  {"API_TOKEN", "BRAVE_KEY"},
	})

	// A broadcast grant of a name two agents declare gets named.
	var buf bytes.Buffer
	warnSecretShapes(&buf, tree, "oncall", ScopedSecrets{"": {"API_TOKEN": "v"}}, nil)
	if !strings.Contains(buf.String(), "API_TOKEN") || !strings.Contains(buf.String(), "brave-tools") {
		t.Errorf("no duplicate-declaration warning: %q", buf.String())
	}

	// The same shape scoped to one agent is exactly the fix; no warning.
	buf.Reset()
	warnSecretShapes(&buf, tree, "oncall", ScopedSecrets{"sentry-tools": {"API_TOKEN": "v"}}, nil)
	if strings.Contains(buf.String(), "declare secret") {
		t.Errorf("scoped grant should not warn about duplicates: %q", buf.String())
	}

	// A scope that names no agent in the run is called out.
	buf.Reset()
	warnSecretShapes(&buf, tree, "oncall", ScopedSecrets{"sentrytools-typo": {"API_TOKEN": "v"}}, nil)
	if !strings.Contains(buf.String(), "sentrytools-typo") {
		t.Errorf("no unknown-scope warning: %q", buf.String())
	}

	// The root's own name is a valid scope.
	buf.Reset()
	warnSecretShapes(&buf, tree, "oncall", ScopedSecrets{"oncall": {"X": "v"}}, nil)
	if buf.Len() != 0 {
		t.Errorf("root-scoped grant warned unexpectedly: %q", buf.String())
	}
}

func TestWarnSecretShapes_SingleNonRootDeclarerWarns(t *testing.T) {
	// A broadcast secret that only ONE sub-agent declares must still warn: one
	// malicious declarer is enough to harvest it, so the trigger is any non-root
	// declarer, not just two or more.
	tree := treeWith(nil, map[string][]string{"sentry-tools": {"SOLO_TOKEN"}})
	var buf bytes.Buffer
	warnSecretShapes(&buf, tree, "oncall", ScopedSecrets{"": {"SOLO_TOKEN": "v"}}, nil)
	if !strings.Contains(buf.String(), "SOLO_TOKEN") || !strings.Contains(buf.String(), "sentry-tools") {
		t.Errorf("single sub-agent declarer of a broadcast secret was not warned: %q", buf.String())
	}
}

func TestWarnSecretShapes_ConfigBoundScopeIsQuiet(t *testing.T) {
	// A scope injected by a per-agent config binding (config secrets set) is
	// global operator config: it legitimately names agents other runs use, so
	// its absence from this run is not the typo the --secret warning exists
	// to catch.
	tree := treeWith(nil, map[string][]string{"sentry-tools": {"API_TOKEN"}})
	var buf bytes.Buffer
	warnSecretShapes(&buf, tree, "oncall", ScopedSecrets{"notes": {"STRIPE_SECRET_KEY": "v"}}, map[string]bool{"notes": true})
	if buf.Len() != 0 {
		t.Errorf("config-bound scope warned unexpectedly: %q", buf.String())
	}
}

func TestWarnSecretShapes_RootOnlyBroadcastIsQuiet(t *testing.T) {
	// When only the root declares a broadcast secret, no sub-agent can reach it,
	// so there is nothing to warn about.
	tree := treeWith([]string{"ROOT_ONLY"}, nil)
	var buf bytes.Buffer
	warnSecretShapes(&buf, tree, "oncall", ScopedSecrets{"": {"ROOT_ONLY": "v"}}, nil)
	if strings.Contains(buf.String(), "declare secret") {
		t.Errorf("root-only broadcast secret warned unexpectedly: %q", buf.String())
	}
}
