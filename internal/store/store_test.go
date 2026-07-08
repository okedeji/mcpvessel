package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/okedeji/agentcage/internal/reference"
)

// newTestStore roots the store at a temp AGENTCAGE_HOME to keep tests off the
// real ~/.agentcage.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestStore_PutThenGetByRef(t *testing.T) {
	s := newTestStore(t)
	const hash = "sha256:abc123"

	dst := s.PathFor(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir bundles: %v", err)
	}
	if err := os.WriteFile(dst, []byte("bundle bytes"), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	ref, err := reference.Parse("@okedeji/researcher:0.1")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := s.Tag(ref, hash); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	got, ok, err := s.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("tagged ref did not resolve")
	}
	if got != dst {
		t.Errorf("Get = %q, want %q", got, dst)
	}
}

func TestList_TaggedAndUntagged(t *testing.T) {
	s := newTestStore(t)
	write := func(hash string) {
		dst := s.PathFor(hash)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(dst, []byte("bundle bytes"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write("sha256:tagged1")
	write("sha256:untagged1")
	ref, err := reference.Parse("@okedeji/researcher:0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tag(ref, "sha256:tagged1"); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]string{} // hash -> ref
	for _, e := range entries {
		got[e.Hash] = e.Ref
	}
	// Default-registry refs read back as @org/name shorthand, not the ghcr host.
	if got["sha256:tagged1"] != "@okedeji/researcher:0.1" {
		t.Errorf("tagged bundle ref = %q, want the @org/name shorthand", got["sha256:tagged1"])
	}
	if _, ok := got["sha256:untagged1"]; !ok {
		t.Error("untagged bundle missing from the listing")
	}
	if got["sha256:untagged1"] != "" {
		t.Errorf("untagged bundle ref = %q, want empty", got["sha256:untagged1"])
	}
}

func TestList_EmptyStore(t *testing.T) {
	newTestStore(t)
	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("List on an empty store = %v, want none", entries)
	}
}

func TestStore_FindByHashFullAndPrefix(t *testing.T) {
	s := newTestStore(t)
	const hash = "sha256:abc123def456"
	dst := s.PathFor(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir bundles: %v", err)
	}
	if err := os.WriteFile(dst, []byte("bundle bytes"), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	for _, q := range []string{hash, "sha256:abc1"} {
		got, ok, err := s.FindByHash(q)
		if err != nil {
			t.Fatalf("FindByHash(%q): %v", q, err)
		}
		if !ok || got != dst {
			t.Errorf("FindByHash(%q) = (%q, %v), want (%q, true)", q, got, ok, dst)
		}
	}

	if _, ok, err := s.FindByHash("sha256:nomatch"); err != nil || ok {
		t.Errorf("FindByHash(miss) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestStore_FindByHashAmbiguousPrefix(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.Dir(), "bundles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundles: %v", err)
	}
	for _, name := range []string{"sha256-abc111.agent", "sha256-abc222.agent"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, _, err := s.FindByHash("sha256:abc"); err == nil {
		t.Fatal("ambiguous prefix should error")
	}
}

func TestStore_GetUnknownRef(t *testing.T) {
	s := newTestStore(t)
	ref, err := reference.Parse("@okedeji/missing:9.9")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if _, ok, err := s.Get(ref); err != nil || ok {
		t.Errorf("Get unknown ref = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

// Dangling index entry: the ref resolves to a hash whose bundle was removed.
func TestStore_GetTagWithMissingBundle(t *testing.T) {
	s := newTestStore(t)
	ref, err := reference.Parse("@okedeji/researcher:0.1")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := s.Tag(ref, "sha256:deadbeef"); err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if _, ok, err := s.Get(ref); err != nil || ok {
		t.Errorf("Get with missing bundle = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestStore_TagRequiresVersion(t *testing.T) {
	s := newTestStore(t)
	ref, err := reference.Parse("@okedeji/researcher")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := s.Tag(ref, "sha256:abc"); err == nil {
		t.Fatal("Tag without a version should error")
	}
}
