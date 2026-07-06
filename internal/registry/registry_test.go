package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/reference"
)

// realBundle builds a minimal but valid .agent so packBundle can read its
// manifest (it pins the OCI created annotation to the bundle's built_at).
func realBundle(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Agentfile"), []byte("FROM x\nENTRYPOINT y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "agent.agent")
	if err := bundle.Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return out
}

func TestPackBundle_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	bundlePath := realBundle(t)
	want, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}

	desc, err := packBundle(ctx, store, "0.1", bundlePath, nil)
	if err != nil {
		t.Fatalf("packBundle: %v", err)
	}
	if desc.ArtifactType != ArtifactType {
		t.Errorf("ArtifactType = %q, want %q", desc.ArtifactType, ArtifactType)
	}

	// content.ReadAll inside fetchBundle verifies the blob digest, so a
	// successful fetch proves integrity.
	got, manifestDesc, err := fetchBundle(ctx, store, "0.1")
	if err != nil {
		t.Fatalf("fetchBundle: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("fetched bytes differ from the pushed bundle")
	}
	if manifestDesc.Digest != desc.Digest {
		t.Errorf("fetched digest = %s, want %s", manifestDesc.Digest, desc.Digest)
	}
}

func TestBundleDigest_DeterministicAndMatchesPush(t *testing.T) {
	bundlePath := realBundle(t)

	d1, err := BundleDigest(bundlePath)
	if err != nil {
		t.Fatalf("BundleDigest: %v", err)
	}
	d2, err := BundleDigest(bundlePath)
	if err != nil {
		t.Fatalf("BundleDigest (again): %v", err)
	}
	if d1 != d2 {
		t.Errorf("BundleDigest is not deterministic: %s vs %s", d1, d2)
	}

	// The locally computed digest must equal what a push produces, so a USES
	// lock made locally stays valid once the dependency is pushed.
	desc, err := packBundle(context.Background(), memory.New(), "0.1", bundlePath, nil)
	if err != nil {
		t.Fatalf("packBundle: %v", err)
	}
	if d1 != desc.Digest.String() {
		t.Errorf("BundleDigest %s != push digest %s", d1, desc.Digest.String())
	}
}

func TestPackBundle_StampsOwnershipForPublish(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	name := "io.github.me/x"

	desc, err := packBundle(ctx, store, "0.1", realBundle(t), map[string]string{mcpServerNameAnnotation: name})
	if err != nil {
		t.Fatalf("packBundle: %v", err)
	}

	mb, err := content.FetchAll(ctx, store, desc)
	if err != nil {
		t.Fatal(err)
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatal(err)
	}
	if man.Annotations[mcpServerNameAnnotation] != name {
		t.Errorf("manifest annotation = %q, want %q", man.Annotations[mcpServerNameAnnotation], name)
	}

	// The registry reads the marker from the config's Labels, so that is what
	// must carry it, not only the manifest annotation.
	cb, err := content.FetchAll(ctx, store, man.Config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(cb, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Config.Labels[mcpServerNameAnnotation] != name {
		t.Errorf("config label = %q, want %q", cfg.Config.Labels[mcpServerNameAnnotation], name)
	}
}

func TestSeedCache_MakesPullALocalHit(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("AGENTCAGE_HOME", home)

	bundlePath := realBundle(t)
	digest, err := BundleDigest(bundlePath)
	if err != nil {
		t.Fatalf("BundleDigest: %v", err)
	}

	if err := SeedCache(digest, bundlePath); err != nil {
		t.Fatalf("SeedCache: %v", err)
	}
	c := &Client{cacheDir: filepath.Join(home, "cache")}

	// A digest-pinned Pull short-circuits from the cache with no network, so a
	// seeded bundle resolves offline.
	ref, err := reference.Parse("ghcr.io/x/y@" + digest)
	if err != nil {
		t.Fatal(err)
	}
	path, gotDigest, err := c.Pull(ctx, ref)
	if err != nil {
		t.Fatalf("Pull after seed: %v", err)
	}
	if gotDigest != digest {
		t.Errorf("pulled digest = %s, want %s", gotDigest, digest)
	}
	if path != c.cachePath(digest) {
		t.Errorf("pulled path = %s, want the cache path", path)
	}
}

func TestFetchBundle_RejectsNonBundleManifest(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// A manifest whose only layer is not the agentcage bundle media type must
	// be rejected: it is some other OCI artifact, not one of ours.
	blob := content.NewDescriptorFromBytes("application/octet-stream", []byte("not a bundle"))
	if err := store.Push(ctx, blob, bytes.NewReader([]byte("not a bundle"))); err != nil {
		t.Fatal(err)
	}
	desc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "application/vnd.example", oras.PackManifestOptions{
		Layers: []ocispec.Descriptor{blob},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, desc, "0.1"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := fetchBundle(ctx, store, "0.1"); err == nil || !bytes.Contains([]byte(err.Error()), []byte("not an agentcage bundle")) {
		t.Fatalf("err = %v, want a not-an-agentcage-bundle rejection", err)
	}
}

func TestCachePath_DigestIsFilesystemSafe(t *testing.T) {
	c := &Client{cacheDir: "/home/u/.agentcage/cache"}
	got := c.cachePath("sha256:abc123")
	want := filepath.Join("/home/u/.agentcage/cache", "bundles", "sha256-abc123.agent")
	if got != want {
		t.Errorf("cachePath = %q, want %q", got, want)
	}
}
