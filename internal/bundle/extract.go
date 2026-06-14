package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Extract opens an .agent file at bundlePath and writes its source tree
// entries into destDir. Returns the parsed manifest as a convenience
// so callers do not have to re-open the file.
//
// destDir must already exist. Files are written with the modes the
// bundle recorded (currently 0o644). The bundle's "files/" prefix is
// stripped so destDir mirrors the layout the author had at build time.
//
// Path traversal is guarded: any entry whose name escapes destDir
// (because of "../" or an absolute path) errors out before any write,
// without leaving the destination partially populated. Bundles from
// untrusted sources can therefore be safely extracted into temp dirs.
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
			// can survive future additions to the bundle format.
		}
	}

	if manifest == nil {
		return nil, fmt.Errorf("bundle %s is missing %s", bundlePath, manifestFilename)
	}

	// Re-hash the extracted tree against the manifest's files_hash. A registry
	// pull is digest-verified by the OCI client, but a local .agent file is
	// not, so this is where a corrupted or tampered source tree is caught
	// before any of it is built or run. The hash cannot stop an attacker who
	// rewrites files_hash to match, only a signature can, but it catches
	// corruption and a tamper that left the manifest alone. An empty hash is a
	// pre-hash bundle with nothing to check against, not a pass.
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

// manifestFilename and filesPrefix mirror the layout Build produces.
// Centralizing them keeps the read and write paths in sync.
const (
	manifestFilename = "manifest.json"
	filesPrefix      = "files/"
)

// ReadManifest reads only the manifest.json entry of an .agent bundle.
// Cheaper than Extract when the caller only needs to inspect bundle
// metadata (for example, to look up the main tool before deciding
// whether to invoke run or fall through to a "this is a tool
// collection" error).
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

func decodeManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// extractFile writes one bundle entry into destAbs/rel. Refuses to
// write outside destAbs.
func extractFile(tr *tar.Reader, hdr *tar.Header, destAbs, rel string) error {
	// Reject any path that escapes destAbs after cleaning. This
	// covers "../", absolute paths, and symlink-shaped entries.
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
		// Skip symlinks, devices, FIFOs, etc. Bundles are source
		// trees; anything else is not what we packaged and we
		// refuse to materialize it.
	}
	return nil
}
