// Package registry pushes and pulls .agent bundles as OCI artifacts: one
// gzip-tar layer under a manifest whose artifactType marks it mcpvessel's.
// Auth reuses the operator's stored OCI credentials. Pulls are cached by
// manifest digest; a repeated digest pull never touches the network.
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

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/reference"
)

const (
	// BundleMediaType is the OCI layer media type for a packed .agent bundle.
	// Changing it strands every bundle already pushed under the old type.
	BundleMediaType = "application/vnd.mcpvessel.bundle.v1+tar+gzip"

	// ArtifactType marks the bundle's OCI manifest as an mcpvessel bundle.
	ArtifactType = "application/vnd.mcpvessel.bundle.v1"

	// mcpServerNameAnnotation is the ownership annotation the MCP Registry
	// requires: the artifact must name the reverse-DNS server it belongs to.
	// Push stamps it on GHCR refs, whose reverse-DNS name is derivable.
	mcpServerNameAnnotation = "io.modelcontextprotocol.server.name"
)

// Client talks to remote OCI registries. The auth client is built once and
// reused across repositories.
type Client struct {
	cacheDir string
	auth     remote.Client

	// Notify, when set, receives human-readable signature notices (a new
	// pin, a verified pull, an unsigned bundle). Enforcement happens either
	// way; this only controls whether anyone hears about it.
	Notify func(format string, args ...any)
}

// New builds a Client with credential-store auth and the default cache
// (~/.mcpvessel/cache). An unreadable credential store is an error, not a
// silent fall-through to anonymous access.
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

// Push uploads the bundle at bundlePath to ref and returns the pushed
// manifest's digest.
func (c *Client) Push(ctx context.Context, ref reference.Reference, bundlePath string) (string, error) {
	if ref.Tag == "" {
		return "", fmt.Errorf("push %s: a tag is required", ref.Original)
	}
	repo, err := c.repository(ref)
	if err != nil {
		return "", err
	}

	// A GHCR ref is publish-bound: stamp the ownership annotation the MCP
	// Registry checks.
	var annotations map[string]string
	if name, ok := ref.ReverseDNSName(); ok {
		annotations = map[string]string{mcpServerNameAnnotation: name}
	}

	desc, err := packBundle(ctx, repo, ref.Tag, bundlePath, annotations)
	if err != nil {
		return "", fmt.Errorf("pushing %s: %w", ref.OCIRef(), err)
	}
	return desc.Digest.String(), nil
}

// Resolve reports the digest a reference currently points at without
// downloading the bundle.
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

// IsBundleArtifact reports whether ref names a published mcpvessel bundle
// rather than a runnable container image: the manifest's artifact type, with
// the bundle layer's media type as the fallback for a registry that strips
// artifact types. Only the manifest is fetched, no layers, so the check is
// cheap enough to run before deciding how to treat an OCI source.
func (c *Client) IsBundleArtifact(ctx context.Context, ref reference.Reference) (bool, error) {
	repo, err := c.repository(ref)
	if err != nil {
		return false, err
	}
	desc, err := repo.Resolve(ctx, resolveTarget(ref))
	if err != nil {
		return false, fmt.Errorf("resolving %s: %w", ref.OCIRef(), err)
	}
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return false, fmt.Errorf("fetching %s manifest: %w", ref.OCIRef(), err)
	}
	manifestBytes, err := content.ReadAll(rc, desc)
	_ = rc.Close()
	if err != nil {
		return false, fmt.Errorf("reading %s manifest: %w", ref.OCIRef(), err)
	}
	return manifestIsBundle(manifestBytes), nil
}

// manifestIsBundle classifies raw manifest bytes: an mcpvessel bundle by
// artifact type, or by a bundle layer when a registry strips artifact types.
// Anything that is not an OCI image manifest (an index, junk) is not one.
func manifestIsBundle(manifestBytes []byte) bool {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return false
	}
	if manifest.ArtifactType == ArtifactType {
		return true
	}
	_, ok := bundleLayer(manifest.Layers)
	return ok
}

// Pull fetches the bundle into the local cache and returns its path plus the
// resolved manifest digest. A digest-pinned reference already in the cache
// returns with no network access.
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

	// Verify before caching: the cache is digest-addressed and immutable, so
	// ingest is the one boundary where signature policy must hold.
	if err := c.verifyPulled(ctx, repo, ref, digest); err != nil {
		return "", "", err
	}

	path := c.cachePath(digest)
	if err := writeCache(path, data); err != nil {
		return "", "", fmt.Errorf("caching %s: %w", ref.OCIRef(), err)
	}
	return path, digest, nil
}

// ResolvePublic reports the digest a reference points at using no
// credentials. It succeeds only for an artifact that is pushed and publicly
// pullable; publish gates MCP Registry advertising on it.
func ResolvePublic(ctx context.Context, ref reference.Reference) (string, error) {
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return "", fmt.Errorf("addressing %s/%s: %w", ref.Registry, ref.Repository, err)
	}
	// No Credential func: an anonymous pull token succeeds only on a public
	// repo. The refusal is the signal; no fallback to stored credentials.
	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	desc, err := repo.Resolve(ctx, resolveTarget(ref))
	if err != nil {
		return "", fmt.Errorf("resolving %s anonymously: %w", ref.OCIRef(), err)
	}
	return desc.Digest.String(), nil
}

// Login validates username/password against the registry host and stores the
// credential in the shared OCI credential store. A rejected credential is an
// error, never stored.
func Login(ctx context.Context, host, username, password string) error {
	if host == "" {
		return fmt.Errorf("login: a registry host is required")
	}
	// AllowPlaintextPut mirrors Docker's own default: when no credential helper
	// is configured (a bare Linux box or a CI runner), fall back to base64 in
	// config.json instead of refusing the login. A machine with a helper still
	// uses it; this only changes the no-helper fallback.
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{AllowPlaintextPut: true})
	if err != nil {
		return fmt.Errorf("opening credential store: %w", err)
	}
	reg, err := remote.NewRegistry(host)
	if err != nil {
		return fmt.Errorf("addressing registry %s: %w", host, err)
	}
	// credentials.Login validates then stores; it uses its own client, so
	// reg.Client stays nil.
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

// resolveTarget is the tag-or-digest fragment registry operations take;
// digest wins.
func resolveTarget(ref reference.Reference) string {
	if ref.Digest != "" {
		return ref.Digest
	}
	return ref.Tag
}

// packBundle uploads the bundle blob to dst, packs an OCI manifest over it,
// and tags it when tag is non-empty.
//
// The created annotation is pinned to the bundle's built_at, not wall clock,
// making the manifest digest a deterministic function of the bundle bytes.
// That lets BundleDigest compute, before any push, the exact digest a later
// push produces, so a locally locked USES digest stays valid.
func packBundle(ctx context.Context, dst oras.Target, tag, bundlePath string, annotations map[string]string) (ocispec.Descriptor, error) {
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

	manifestAnnotations := map[string]string{
		ocispec.AnnotationCreated: m.BuiltAt.UTC().Format(time.RFC3339),
	}
	for k, v := range annotations {
		manifestAnnotations[k] = v
	}

	opts := oras.PackManifestOptions{
		Layers:              []ocispec.Descriptor{blob},
		ManifestAnnotations: manifestAnnotations,
	}
	// The MCP Registry reads the ownership marker from the image config's
	// Labels, not the manifest annotations, so a publish-bound push carries a
	// labeled config blob instead of the empty artifact config.
	if len(annotations) > 0 {
		cfgDesc, err := pushConfigWithLabels(ctx, dst, annotations)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		opts.ConfigDescriptor = &cfgDesc
	}
	manifestDesc, err := oras.PackManifest(ctx, dst, oras.PackManifestVersion1_1, ArtifactType, opts)
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

// pushConfigWithLabels uploads an OCI image config carrying the given Labels,
// the same place a Dockerfile LABEL lands.
func pushConfigWithLabels(ctx context.Context, dst oras.Target, labels map[string]string) (ocispec.Descriptor, error) {
	cfg := struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}{}
	cfg.Config.Labels = labels
	data, err := json.Marshal(cfg)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("encoding image config: %w", err)
	}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, data)
	exists, err := dst.Exists(ctx, desc)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("checking config blob: %w", err)
	}
	if !exists {
		if err := dst.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("uploading image config: %w", err)
		}
	}
	return desc, nil
}

// BundleDigest computes, with no network access, the OCI manifest digest the
// bundle would be pushed under. Deterministic and equal to what Push returns.
func BundleDigest(bundlePath string) (string, error) {
	desc, err := packBundle(context.Background(), memory.New(), "", bundlePath, nil)
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// SeedCache writes the bundle into the local pull cache under digest, which
// must be the bundle's own BundleDigest; a mismatched digest hands out the
// wrong bytes for a lock. Package function, not a Client method: seeding is a
// local write that must work without registry credentials.
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

// fetchBundle resolves a reference to its manifest and returns the verified
// bundle layer bytes. content.ReadAll checks digest and size, catching
// corrupted or truncated content before it reaches the cache.
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
		return nil, ocispec.Descriptor{}, fmt.Errorf("manifest has no %s layer: not an mcpvessel bundle", BundleMediaType)
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

func (c *Client) cachePath(digest string) string {
	return bundleCachePath(c.cacheDir, digest)
}

// bundleCachePath maps a digest to its cache path; ':' becomes '-' for
// filename portability.
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
	// Write-then-rename so an interrupted pull never leaves a partial bundle
	// masquerading as a cache hit.
	return os.Rename(tmp, path)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// cacheDir resolves ~/.mcpvessel/cache, honoring VESSEL_HOME.
func cacheDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv(env.Home)); home != "" {
		return filepath.Join(home, "cache"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".mcpvessel", "cache"), nil
}
