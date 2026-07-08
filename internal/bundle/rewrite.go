package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
)

// RewriteManifest replaces a bundle's manifest.json in place, copying every
// files/ entry through byte for byte so files_hash and the content-addressed
// store key are unchanged. This is how a full-suite eval run stamps results
// into a bundle that already shipped. Staged to a temp file and renamed on
// success, same discipline as writeBundle, so an interrupted rewrite never
// leaves a truncated bundle in the store.
func RewriteManifest(bundlePath string, mutate func(*Manifest) error) error {
	in, err := os.Open(bundlePath)
	if err != nil {
		return fmt.Errorf("open bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = in.Close() }()

	gzr, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("gunzip bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)

	tmpPath := bundlePath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating rewrite temp: %w", err)
	}
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	gzw := gzip.NewWriter(out)
	tw := tar.NewWriter(gzw)

	sawManifest := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		if hdr.Name == manifestFilename {
			sawManifest = true
			m, err := decodeManifest(tr)
			if err != nil {
				return err
			}
			if err := mutate(m); err != nil {
				return fmt.Errorf("rewriting manifest: %w", err)
			}
			if err := writeManifestEntry(tw, m); err != nil {
				return err
			}
			continue
		}
		if err := copyEntry(tw, tr, hdr); err != nil {
			return err
		}
	}
	if !sawManifest {
		return fmt.Errorf("bundle %s is missing %s", bundlePath, manifestFilename)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing rewritten bundle: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing rewritten bundle: %w", err)
	}
	if err := os.Rename(tmpPath, bundlePath); err != nil {
		return fmt.Errorf("finalizing rewritten bundle: %w", err)
	}
	committed = true
	return nil
}

// copyEntry writes a tar entry's header and body through unchanged.
func copyEntry(tw *tar.Writer, tr *tar.Reader, hdr *tar.Header) error {
	clone := *hdr
	if err := tw.WriteHeader(&clone); err != nil {
		return fmt.Errorf("writing header for %s: %w", hdr.Name, err)
	}
	if _, err := io.Copy(tw, tr); err != nil {
		return fmt.Errorf("copying body for %s: %w", hdr.Name, err)
	}
	return nil
}
