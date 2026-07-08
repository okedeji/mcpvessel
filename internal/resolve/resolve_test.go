package resolve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/reference"
)

// fakeReg serves pre-built bundles and canned digests keyed by repository:tag.
type fakeReg struct {
	digests map[string]string
	bundles map[string]string
}

func keyOf(ref reference.Reference) string {
	return ref.Repository + ":" + ref.Tag
}

func (f *fakeReg) Resolve(_ context.Context, ref reference.Reference) (string, error) {
	d, ok := f.digests[keyOf(ref)]
	if !ok {
		return "", fmt.Errorf("no such tag: %s", keyOf(ref))
	}
	return d, nil
}

func (f *fakeReg) Pull(_ context.Context, ref reference.Reference) (string, string, error) {
	p, ok := f.bundles[keyOf(ref)]
	if !ok {
		return "", "", fmt.Errorf("no such bundle: %s", keyOf(ref))
	}
	return p, f.digests[keyOf(ref)], nil
}

// buildBundle writes a minimal agent with the given USES lines and returns
// the .agent path.
func buildBundle(t *testing.T, name string, usesLines ...string) string {
	t.Helper()
	src := t.TempDir()
	body := "FROM python:3.12-slim\nMAIN respond\n"
	for _, u := range usesLines {
		body += u + "\n"
	}
	body += "ENTRYPOINT python3 agent.py\n"
	if err := os.WriteFile(filepath.Join(src, "Agentfile"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "agent.py"), []byte("print('x')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), name+".agent")
	if err := bundle.Build(src, out); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return out
}

func TestResolve_LocksDigests(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	reg := &fakeReg{
		digests: map[string]string{"acme/web:1.0.0": "sha256:web", "acme/pdf:2.0.0": "sha256:pdf"},
		bundles: map[string]string{
			"acme/web:1.0.0": buildBundle(t, "web"),
			"acme/pdf:2.0.0": buildBundle(t, "pdf"),
		},
	}
	parent := buildBundle(t, "parent",
		"USES @acme/web:1.0.0",
		"USES @acme/pdf:2.0.0",
	)
	uses := mustUses(t, parent)

	got, err := New(reg).Resolve(context.Background(), uses, Options{ParentKey: "@acme/parent:0.1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := map[string]string{"@acme/web:1.0.0": "sha256:web", "@acme/pdf:2.0.0": "sha256:pdf"}
	for k, v := range want {
		if got.Digests[k] != v {
			t.Errorf("Digests[%s] = %q, want %q", k, got.Digests[k], v)
		}
	}
}

func TestResolve_DetectsCycleBackToParent(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	web := buildBundle(t, "web", "USES @acme/parent:0.1")
	reg := &fakeReg{
		digests: map[string]string{"acme/web:1.0.0": "sha256:web", "acme/parent:0.1": "sha256:parent"},
		bundles: map[string]string{"acme/web:1.0.0": web},
	}
	parent := buildBundle(t, "parent", "USES @acme/web:1.0.0")
	uses := mustUses(t, parent)

	_, err := New(reg).Resolve(context.Background(), uses, Options{ParentKey: "@acme/parent:0.1"})
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want it to mention a cycle", err.Error())
	}
}

func TestResolve_DetectsCycleAmongDependencies(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	a := buildBundle(t, "a", "USES @acme/b:1.0.0")
	b := buildBundle(t, "b", "USES @acme/a:1.0.0")
	reg := &fakeReg{
		digests: map[string]string{"acme/a:1.0.0": "sha256:a", "acme/b:1.0.0": "sha256:b"},
		bundles: map[string]string{"acme/a:1.0.0": a, "acme/b:1.0.0": b},
	}
	parent := buildBundle(t, "parent", "USES @acme/a:1.0.0")
	uses := mustUses(t, parent)

	_, err := New(reg).Resolve(context.Background(), uses, Options{ParentKey: "@acme/parent:0.1"})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
}

func TestResolve_AcyclicGraphPasses(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	leaf := buildBundle(t, "leaf")
	mid := buildBundle(t, "mid", "USES @acme/leaf:1.0.0")
	reg := &fakeReg{
		digests: map[string]string{"acme/mid:1.0.0": "sha256:mid", "acme/leaf:1.0.0": "sha256:leaf"},
		bundles: map[string]string{"acme/mid:1.0.0": mid, "acme/leaf:1.0.0": leaf},
	}
	parent := buildBundle(t, "parent", "USES @acme/mid:1.0.0")
	uses := mustUses(t, parent)

	if _, err := New(reg).Resolve(context.Background(), uses, Options{ParentKey: "@acme/parent:0.1"}); err != nil {
		t.Fatalf("acyclic graph should resolve, got %v", err)
	}
}

func TestResolve_SkipCycleCheckStillLocksDigests(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	// No bundle registered for web: with SkipCycleCheck the walk (and its
	// pull) never runs, and resolution still returns the digest.
	reg := &fakeReg{
		digests: map[string]string{"acme/web:1.0.0": "sha256:web"},
		bundles: map[string]string{},
	}
	parent := buildBundle(t, "parent", "USES @acme/web:1.0.0")
	uses := mustUses(t, parent)

	got, err := New(reg).Resolve(context.Background(), uses, Options{ParentKey: "@acme/parent:0.1", SkipCycleCheck: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Digests["@acme/web:1.0.0"] != "sha256:web" {
		t.Errorf("digest = %q, want sha256:web", got.Digests["@acme/web:1.0.0"])
	}
}

func mustUses(t *testing.T, bundlePath string) []agentfile.Use {
	t.Helper()
	m, err := bundle.ReadManifest(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	return usesFromSpec(m.Agentfile.Uses)
}
