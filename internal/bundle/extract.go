package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Extract writes an .agent file's source tree into destDir, which must
// already exist, and returns the parsed manifest. The "files/" prefix is
// stripped so destDir mirrors the author's layout. Any entry whose name
// escapes destDir ("../" or an absolute path) errors before the write, so
// untrusted bundles extract safely.
func Extract(bundlePath, destDir string) (*Manifest, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gunzip bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("absolute path for %s: %w", destDir, err)
	}

	var manifest *Manifest
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar header: %w", err)
		}

		switch {
		case hdr.Name == manifestFilename:
			m, err := decodeManifest(tr)
			if err != nil {
				return nil, err
			}
			manifest = m
		case strings.HasPrefix(hdr.Name, filesPrefix):
			rel := strings.TrimPrefix(hdr.Name, filesPrefix)
			if rel == "" {
				continue
			}
			if err := extractFile(tr, hdr, destAbs, rel); err != nil {
				return nil, err
			}
		default:
			// Unknown top-level entries are ignored so older readers
			// survive future format additions.
		}
	}

	if manifest == nil {
		return nil, fmt.Errorf("bundle %s is missing %s", bundlePath, manifestFilename)
	}

	// Re-hash the extracted tree against files_hash. A registry pull is
	// digest-verified by the OCI client; a local .agent file is not, so this
	// is where corruption or a tamper that left the manifest alone is caught.
	// Integrity, not authenticity: an attacker who rewrites files_hash to
	// match defeats it, only a signature would not. An empty hash is a
	// pre-hash bundle with nothing to check, not a pass.
	if manifest.FilesHash != "" {
		got, err := hashFiles(destAbs, nil)
		if err != nil {
			return nil, fmt.Errorf("verifying bundle %s: %w", bundlePath, err)
		}
		if got != manifest.FilesHash {
			return nil, fmt.Errorf("bundle %s failed integrity check: extracted files hash %s, manifest records %s", bundlePath, got, manifest.FilesHash)
		}
	}
	return manifest, nil
}

// Layout constants shared by the read and write paths.
const (
	manifestFilename = "manifest.json"
	filesPrefix      = "files/"
)

// ReadManifest reads only the manifest.json entry of an .agent bundle,
// cheaper than Extract when only metadata is needed.
func ReadManifest(bundlePath string) (*Manifest, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gunzip bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar header: %w", err)
		}
		if hdr.Name == manifestFilename {
			return decodeManifest(tr)
		}
	}
	return nil, fmt.Errorf("bundle %s is missing %s", bundlePath, manifestFilename)
}

// ReadSourceFile returns one files/ entry without extracting the tree. rel is
// relative to the source root; a rel that escapes it is rejected before any
// read.
func ReadSourceFile(bundlePath, rel string) ([]byte, error) {
	clean := path.Clean("/" + filepath.ToSlash(rel))
	if clean == "/" {
		return nil, fmt.Errorf("bundle %s: empty source file path", bundlePath)
	}
	want := filesPrefix + strings.TrimPrefix(clean, "/")

	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gunzip bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar header: %w", err)
		}
		if hdr.Name != want {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading %s from bundle %s: %w", rel, bundlePath, err)
		}
		return body, nil
	}
	return nil, fmt.Errorf("bundle %s does not contain %s", bundlePath, rel)
}

func decodeManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// extractFile writes one bundle entry into destAbs/rel, refusing anything
// that escapes destAbs.
func extractFile(tr *tar.Reader, hdr *tar.Header, destAbs, rel string) error {
	// Covers "../", absolute paths, and symlink-shaped entries.
	target := filepath.Join(destAbs, rel)
	cleanTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("absolute path for %s: %w", target, err)
	}
	if cleanTarget != destAbs && !strings.HasPrefix(cleanTarget, destAbs+string(filepath.Separator)) {
		return fmt.Errorf("bundle entry %q escapes destination directory", hdr.Name)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(cleanTarget, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", cleanTarget, err)
		}
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(cleanTarget), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", cleanTarget, err)
		}
		mode := os.FileMode(hdr.Mode) & 0o777
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return fmt.Errorf("create %s: %w", cleanTarget, err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return fmt.Errorf("write %s: %w", cleanTarget, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close %s: %w", cleanTarget, err)
		}
	default:
		// Symlinks, devices, FIFOs: bundles are source trees, refuse to
		// materialize anything else.
	}
	return nil
}
