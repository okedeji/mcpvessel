// Package store keeps built .agent bundles under ~/.agentcage/store.
// Bundles are content-addressed by manifest files_hash under bundles/;
// refs/ maps a reference to that hash. No daemon involved: build writes,
// push/run/call read directly.
package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/reference"
)

// Store is the local bundle store under ~/.agentcage/store.
type Store struct {
	dir string
}

// New opens the store, honoring AGENTCAGE_HOME. The directory is created
// lazily by the first write.
func New() (*Store, error) {
	dir, err := storeDir()
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store root. Build hashes source against this path; it sits
// outside any source tree, so it excludes nothing from the hash.
func (s *Store) Dir() string { return s.dir }

// PathFor returns the bundle path for a files_hash. ':' becomes '-' for
// filename portability, matching the pull cache.
func (s *Store) PathFor(filesHash string) string {
	return filepath.Join(s.dir, "bundles", strings.ReplaceAll(filesHash, ":", "-")+".agent")
}

// Tag points ref at a stored bundle's files_hash.
func (s *Store) Tag(ref reference.Reference, filesHash string) error {
	if ref.Tag == "" {
		return fmt.Errorf("tagging the store needs a version, %s has none", ref.Original)
	}
	path := s.refPath(ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating store ref dir: %w", err)
	}
	// Write-then-rename so an interrupted tag never leaves a partial ref.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(filesHash), 0o644); err != nil {
		return fmt.Errorf("writing store ref: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("finalizing store ref: %w", err)
	}
	return nil
}

// Get resolves a tagged reference to its stored bundle path. ok is false when
// the ref is not indexed or its bundle is missing.
func (s *Store) Get(ref reference.Reference) (bundlePath string, ok bool, err error) {
	if ref.Tag == "" {
		return "", false, nil
	}
	hash, err := os.ReadFile(s.refPath(ref))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading store ref %s: %w", ref.OCIRef(), err)
	}
	path := s.PathFor(strings.TrimSpace(string(hash)))
	if !fileExists(path) {
		return "", false, nil
	}
	return path, true, nil
}

// FindByHash resolves a content hash or unique prefix to its stored bundle.
// An ambiguous prefix errors; ok is false on no match.
func (s *Store) FindByHash(hash string) (bundlePath string, ok bool, err error) {
	prefix := strings.ReplaceAll(hash, ":", "-")
	dir := filepath.Join(s.dir, "bundles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading store bundles: %w", err)
	}
	var match string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".agent") {
			continue
		}
		if !strings.HasPrefix(strings.TrimSuffix(name, ".agent"), prefix) {
			continue
		}
		if match != "" {
			return "", false, fmt.Errorf("content hash %s is ambiguous in the store", hash)
		}
		match = filepath.Join(dir, name)
	}
	if match == "" {
		return "", false, nil
	}
	return match, true, nil
}

// refPath: refs/<registry>/<repository>/<tag>.
func (s *Store) refPath(ref reference.Reference) string {
	return filepath.Join(s.dir, "refs", ref.Registry, filepath.FromSlash(ref.Repository), ref.Tag)
}

// Entry is one (bundle, ref) pairing as List reports it. Ref is empty for a
// bundle stored only by content hash.
type Entry struct {
	Ref  string
	Hash string
	Size int64
}

// List returns every stored bundle, one entry per ref plus a ref-less entry
// for a bundle no tag points at.
func List() ([]Entry, error) {
	s, err := New()
	if err != nil {
		return nil, err
	}
	refs, err := s.refsByHash()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(s.dir, "bundles")
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading store bundles: %w", err)
	}

	var out []Entry
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".agent") {
			continue
		}
		hash := unsanitizeHash(strings.TrimSuffix(name, ".agent"))
		var size int64
		if info, err := f.Info(); err == nil {
			size = info.Size()
		}
		tags := refs[hash]
		if len(tags) == 0 {
			out = append(out, Entry{Hash: hash, Size: size})
			continue
		}
		for _, ref := range tags {
			out = append(out, Entry{Ref: ref, Hash: hash, Size: size})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		return out[i].Hash < out[j].Hash
	})
	return out, nil
}

// refsByHash inverts the ref index: files_hash -> reference strings.
func (s *Store) refsByHash() (map[string][]string, error) {
	root := filepath.Join(s.dir, "refs")
	out := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hash, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading store ref %s: %w", rel, err)
		}
		out[strings.TrimSpace(string(hash))] = append(out[strings.TrimSpace(string(hash))], refFromRelPath(rel))
		return nil
	})
	if os.IsNotExist(err) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading store refs: %w", err)
	}
	return out, nil
}

// refFromRelPath rebuilds a display reference from <registry>/<repo...>/<tag>.
func refFromRelPath(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 3 {
		return rel
	}
	return reference.Reference{
		Registry:   parts[0],
		Repository: strings.Join(parts[1:len(parts)-1], "/"),
		Tag:        parts[len(parts)-1],
	}.Display()
}

// unsanitizeHash reverses PathFor's ':' -> '-'. Only the first '-' is
// restored; a sha256 hex body contains none.
func unsanitizeHash(name string) string {
	return strings.Replace(name, "-", ":", 1)
}

// CopyTo writes a copy of the bundle at src to dst.
func CopyTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening stored bundle: %w", err)
	}
	defer func() { _ = in.Close() }()

	if dir := filepath.Dir(dst); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating output directory: %w", err)
		}
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying bundle: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// storeDir resolves ~/.agentcage/store, honoring AGENTCAGE_HOME.
func storeDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv(env.Home)); home != "" {
		return filepath.Join(home, "store"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".agentcage", "store"), nil
}
