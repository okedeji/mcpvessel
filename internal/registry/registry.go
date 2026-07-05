// Package registry pushes and pulls .agent bundles as OCI artifacts and
// resolves reference tags to the digests the manifest lockfile records.
//
// A bundle ships as a single OCI layer (the gzip-tar .agent file) under
// an image manifest whose artifactType marks it as agentcage's. Any OCI
// registry can store, dedupe, and serve it without understanding what a
// bundle is. Authentication reuses the operator's stored OCI registry
// credentials, so there is no agentcage-specific login.
//
// Pulls are content-addressed: a bundle fetched once lands in the local
// cache keyed by its manifest digest, and a later pull of the same digest
// never touches the network.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/reference"
)

const (
	// BundleMediaType is the OCI layer media type for a packed .agent
	// bundle. Changing this string strands every bundle already pushed
	// under the old type.
	BundleMediaType = "application/vnd.agentcage.bundle.v1+tar+gzip"

	// ArtifactType marks the bundle's OCI manifest so a registry browser
	// can tell an agentcage bundle from an ordinary container image.
	ArtifactType = "application/vnd.agentcage.bundle.v1"
)

// Client talks to remote OCI registries on the operator's behalf. The
// auth client is built once from the stored OCI credentials and reused
// across repositories so a multi-pull resolve does not re-read config per call.
type Client struct {
	cacheDir string
	auth     remote.Client
}

// New builds a Client with credential-store authentication and the default
// on-disk cache (~/.agentcage/cache). It fails closed: an unreadable
// credential store is an error, not a silent fall-through to anonymous
// access, so a private pull does not surprise the operator with a 401 three
// layers down.
func New() (*Client, error) {
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading registry credentials: %w", err)
	}
	cache, err := cacheDir()
	if err != nil {
		return nil, err
	}
	return &Client{
		cacheDir: cache,
		auth: &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(store),
		},
	}, nil
}

// Push uploads the bundle at bundlePath to ref and returns the digest of
// the pushed manifest. The digest is what callers lock into a parent's
// manifest so later pulls fetch exactly this artifact.
func (c *Client) Push(ctx context.Context, ref reference.Reference, bundlePath string) (string, error) {
	if ref.Tag == "" {
		return "", fmt.Errorf("push %s: a tag is required", ref.Original)
	}
	repo, err := c.repository(ref)
	if err != nil {
		return "", err
	}
	desc, err := packBundle(ctx, repo, ref.Tag, bundlePath)
	if err != nil {
		return "", fmt.Errorf("pushing %s: %w", ref.OCIRef(), err)
	}
	return desc.Digest.String(), nil
}

// Resolve reports the digest a reference currently points at without
// downloading the bundle. The build-time USES resolver uses this to lock
// a tag into the manifest.
func (c *Client) Resolve(ctx context.Context, ref reference.Reference) (string, error) {
	repo, err := c.repository(ref)
	if err != nil {
		return "", err
	}
	desc, err := repo.Resolve(ctx, resolveTarget(ref))
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref.OCIRef(), err)
	}
	return desc.Digest.String(), nil
}

// Pull fetches the bundle ref names into the local cache and returns its
// path plus the resolved manifest digest. A reference already pinned to a
// digest that is present in the cache returns immediately with no network
// access at all.
func (c *Client) Pull(ctx context.Context, ref reference.Reference) (bundlePath, digest string, err error) {
	if ref.Digest != "" {
		if path := c.cachePath(ref.Digest); fileExists(path) {
			return path, ref.Digest, nil
		}
	}

	repo, err := c.repository(ref)
	if err != nil {
		return "", "", err
	}

	data, manifestDesc, err := fetchBundle(ctx, repo, resolveTarget(ref))
	if err != nil {
		return "", "", fmt.Errorf("pulling %s: %w", ref.OCIRef(), err)
	}
	digest = manifestDesc.Digest.String()

	path := c.cachePath(digest)
	if err := writeCache(path, data); err != nil {
		return "", "", fmt.Errorf("caching %s: %w", ref.OCIRef(), err)
	}
	return path, digest, nil
}

// Login validates username/password against the registry host and stores
// the credential in the shared OCI credential store, so later push and pull
// authenticate without re-entering it. A credential the registry rejects
// is an error: a login that silently failed is worse than no login.
func Login(ctx context.Context, host, username, password string) error {
	if host == "" {
		return fmt.Errorf("login: a registry host is required")
	}
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return fmt.Errorf("opening credential store: %w", err)
	}
	reg, err := remote.NewRegistry(host)
	if err != nil {
		return fmt.Errorf("addressing registry %s: %w", host, err)
	}
	// credentials.Login validates the credential against the registry and
	// stores it. It uses its own client internally, so reg.Client stays
	// nil; the credential we pass is applied to the validating request.
	cred := auth.Credential{Username: username, Password: password}
	if err := credentials.Login(ctx, store, reg, cred); err != nil {
		return fmt.Errorf("logging in to %s: %w", host, err)
	}
	return nil
}

// repository wires a remote repository handle with the shared auth client.
func (c *Client) repository(ref reference.Reference) (*remote.Repository, error) {
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, fmt.Errorf("addressing %s/%s: %w", ref.Registry, ref.Repository, err)
	}
	repo.Client = c.auth
	return repo, nil
}

// resolveTarget is the tag-or-digest fragment registry operations take.
// Digest wins so a locked reference fetches exactly what it pinned.
func resolveTarget(ref reference.Reference) string {
	if ref.Digest != "" {
		return ref.Digest
	}
	return ref.Tag
}

// packBundle uploads the bundle blob to dst, packs an OCI image manifest that
// references it, and (when tag is non-empty) tags the manifest, returning the
// manifest descriptor. dst is an oras.Target so the body runs against a remote
// repository (Push), an in-memory store (BundleDigest), or a test store.
//
// The manifest's created annotation is pinned to the bundle's own build time,
// not the current wall clock, so the manifest digest is a deterministic
// function of the bundle bytes. That is what lets BundleDigest compute, before
// any push, the exact digest a later push will produce: a locally locked USES
// digest stays valid once its dependency is pushed.
func packBundle(ctx context.Context, dst oras.Target, tag, bundlePath string) (ocispec.Descriptor, error) {
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("reading bundle %s: %w", bundlePath, err)
	}
	m, err := bundle.ReadManifest(bundlePath)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("reading bundle manifest %s: %w", bundlePath, err)
	}
	blob := content.NewDescriptorFromBytes(BundleMediaType, data)

	exists, err := dst.Exists(ctx, blob)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("checking blob: %w", err)
	}
	if !exists {
		if err := dst.Push(ctx, blob, bytes.NewReader(data)); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("uploading bundle blob: %w", err)
		}
	}

	manifestDesc, err := oras.PackManifest(ctx, dst, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers: []ocispec.Descriptor{blob},
		ManifestAnnotations: map[string]string{
			ocispec.AnnotationCreated: m.BuiltAt.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("packing manifest: %w", err)
	}
	if tag != "" {
		if err := dst.Tag(ctx, manifestDesc, tag); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("tagging %s: %w", tag, err)
		}
	}
	return manifestDesc, nil
}

// BundleDigest computes the OCI manifest digest the bundle would be pushed
// under, without any network access, by packing its manifest against an
// in-memory store. It is deterministic and equals the digest Push returns, so a
// locally built bundle can be locked into a parent's USES and later pulled back
// by that same digest. The build seeds the pull cache under it (SeedCache) so
// the runtime resolves a local dependency without a registry.
func BundleDigest(bundlePath string) (string, error) {
	desc, err := packBundle(context.Background(), memory.New(), "", bundlePath)
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// SeedCache writes the bundle at bundlePath into the local pull cache under
// digest, so a later Pull of that digest is a local hit with no network. The
// build uses it to make a locally built dependency reachable by the runtime,
// which pulls sub-agents by their locked digest. digest must be the bundle's
// own BundleDigest; seeding a mismatched digest would hand the runtime the
// wrong bytes for a lock.
//
// It is a package function, not a Client method, because seeding is a local
// filesystem write that a purely local build must do without registry
// credentials.
func SeedCache(digest, bundlePath string) error {
	dir, err := cacheDir()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return fmt.Errorf("reading bundle %s: %w", bundlePath, err)
	}
	if err := writeCache(bundleCachePath(dir, digest), data); err != nil {
		return fmt.Errorf("seeding cache for %s: %w", digest, err)
	}
	return nil
}

// fetchBundle resolves a reference to its manifest, finds the single
// bundle layer, and returns the verified bundle bytes. content.ReadAll
// checks the digest and size, so a registry that serves corrupted or
// truncated content is caught before the bytes reach the cache.
func fetchBundle(ctx context.Context, src oras.ReadOnlyTarget, ref string) ([]byte, ocispec.Descriptor, error) {
	manifestDesc, err := src.Resolve(ctx, ref)
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("resolving manifest: %w", err)
	}
	mrc, err := src.Fetch(ctx, manifestDesc)
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("fetching manifest: %w", err)
	}
	manifestBytes, err := content.ReadAll(mrc, manifestDesc)
	_ = mrc.Close()
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("reading manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("decoding manifest: %w", err)
	}

	layer, ok := bundleLayer(manifest.Layers)
	if !ok {
		return nil, ocispec.Descriptor{}, fmt.Errorf("manifest has no %s layer: not an agentcage bundle", BundleMediaType)
	}

	brc, err := src.Fetch(ctx, layer)
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("fetching bundle blob: %w", err)
	}
	data, err := content.ReadAll(brc, layer)
	_ = brc.Close()
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("reading bundle blob: %w", err)
	}
	return data, manifestDesc, nil
}

func bundleLayer(layers []ocispec.Descriptor) (ocispec.Descriptor, bool) {
	for _, l := range layers {
		if l.MediaType == BundleMediaType {
			return l, true
		}
	}
	return ocispec.Descriptor{}, false
}

// cachePath is where a bundle of the given digest lives on disk.
func (c *Client) cachePath(digest string) string {
	return bundleCachePath(c.cacheDir, digest)
}

// bundleCachePath is where a bundle of the given digest lives under cacheDir.
// The ':' in a digest is not portable in a filename, so sha256:abc becomes
// sha256-abc.
func bundleCachePath(cacheDir, digest string) string {
	return filepath.Join(cacheDir, "bundles", strings.ReplaceAll(digest, ":", "-")+".agent")
}

func writeCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	// Rename-on-success so an interrupted pull never leaves a partial
	// bundle masquerading as a complete cache hit.
	return os.Rename(tmp, path)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// cacheDir is the root of agentcage's on-disk cache, ~/.agentcage/cache,
// overridable via AGENTCAGE_HOME for operators who keep state elsewhere.
func cacheDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv(env.Home)); home != "" {
		return filepath.Join(home, "cache"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".agentcage", "cache"), nil
}
