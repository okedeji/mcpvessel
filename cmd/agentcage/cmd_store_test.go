package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/store"
)

// buildStoredBundle writes a minimal agent into the store and returns its
// files_hash; the caller tags it.
func buildStoredBundle(t *testing.T, st *store.Store) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Agentfile"), []byte("FROM x\nMAIN respond\nENTRYPOINT y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := bundle.HashSource(src, st.Dir())
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	if err := bundle.Build(src, st.PathFor(hash)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return hash
}

func TestStoreFirstResolver_ResolvesLocalWithoutRegistry(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	hash := buildStoredBundle(t, st)
	ref, err := reference.Parse("@me/child:1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tag(ref, hash); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	// reg is nil: a local hit must not need the registry at all.
	r := storeFirstResolver{store: st, reg: nil, regErr: errors.New("registry off")}
	got, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve local: %v", err)
	}
	want, err := registry.BundleDigest(st.PathFor(hash))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Resolve = %s, want the local bundle digest %s", got, want)
	}
}

func TestStoreFirstResolver_MissWithNoRegistryErrors(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	miss, err := reference.Parse("@me/absent:1.0")
	if err != nil {
		t.Fatal(err)
	}
	r := storeFirstResolver{store: st, reg: nil, regErr: errors.New("registry off")}
	if _, err := r.Resolve(context.Background(), miss); err == nil || !strings.Contains(err.Error(), "not in the local store") {
		t.Fatalf("err = %v, want a clear not-local-and-no-registry error", err)
	}
}

func TestLoadBundle_RoundTrip(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())

	// A bundle handed to us as a loose file, built elsewhere.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Agentfile"), []byte("FROM x\nMAIN respond\nENTRYPOINT y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "child.agent")
	if err := bundle.Build(src, file); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var out bytes.Buffer
	if err := loadBundle(&out, file, "@me/child:1.0"); err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	if !strings.Contains(out.String(), "Loaded") {
		t.Errorf("output = %q, want a load confirmation", out.String())
	}

	// The imported ref now resolves from the store, with no registry.
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := reference.Parse("@me/child:1.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Get(ref); err != nil || !ok {
		t.Fatalf("imported ref did not resolve: ok=%v err=%v", ok, err)
	}
}
