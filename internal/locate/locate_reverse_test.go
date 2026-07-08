package locate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/store"
)

func TestReverseDNSName(t *testing.T) {
	yes := []string{"io.github.user/filesystem", "com.example/weather", "io.github.Digital-Defiance/mcp-filesystem"}
	for _, a := range yes {
		if _, ok := RegistryName(a); !ok {
			t.Errorf("RegistryName(%q) = false, want true", a)
		}
	}
	no := []string{"ghcr.io/org/name:0.1", "@org/name:0.1", "io.github.user/fs:0.1", "sha256:abcdef", "./x.agent"}
	for _, a := range no {
		if _, ok := RegistryName(a); ok {
			t.Errorf("RegistryName(%q) = true, want false", a)
		}
	}
}

func TestBundle_ResolvesReverseDNSThroughStore(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())

	// The MCP Registry resolves the reverse-DNS name to a GHCR artifact.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0.1/servers" {
			_ = json.NewEncoder(w).Encode(map[string]any{"servers": []map[string]any{{"server": map[string]any{
				"name":     "io.github.me/fs",
				"packages": []map[string]any{{"registryType": "oci", "identifier": "ghcr.io/me/fs", "version": "0.1"}},
			}}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	t.Setenv(env.MCPRegistry, ts.URL)

	// Seed the store with that artifact so resolution lands locally; no real
	// pull is needed to prove the name reached the right ref.
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Agentfile"), []byte("FROM x\nMAIN respond\nENTRYPOINT y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := bundle.HashSource(src, st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Build(src, st.PathFor(hash)); err != nil {
		t.Fatal(err)
	}
	ref, err := reference.Parse("ghcr.io/me/fs:0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tag(ref, hash); err != nil {
		t.Fatal(err)
	}

	got, err := Bundle(context.Background(), "io.github.me/fs")
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if got.Path != st.PathFor(hash) {
		t.Errorf("resolved path = %q, want the stored bundle", got.Path)
	}
}
