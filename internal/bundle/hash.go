package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hashFiles walks root and returns a sha256 over a canonical encoding of
// every regular file beneath it.
//
// The encoding is `<relpath>\0<content>\0` for each file, with files
// sorted by relative path. Two equivalent source trees produce the same
// hash; a renamed or modified file changes it.
//
// skip is checked against the path relative to root. It is meant for
// excluding entries like ".git" and the build output itself.
func hashFiles(root string, skip func(rel string) bool) (string, error) {
	paths, err := walkFiles(root, skip)
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, rel := range paths {
		// Path component: written before content so a rename changes
		// the hash even if content is unchanged.
		h.Write([]byte(rel))
		h.Write([]byte{0})

		abs := filepath.Join(root, rel)
		f, err := os.Open(abs)
		if err != nil {
			return "", fmt.Errorf("hashing %s: %w", rel, err)
		}
		_, err = io.Copy(h, f)
		closeErr := f.Close()
		if err != nil {
			return "", fmt.Errorf("hashing %s: %w", rel, err)
		}
		if closeErr != nil {
			return "", fmt.Errorf("hashing %s: %w", rel, closeErr)
		}
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// walkFiles returns the relative paths (slash-separated, forward slashes
// on every OS) of every regular file under root, filtered by skip.
func walkFiles(root string, skip func(rel string) bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// Normalize path separators so the same source tree hashes
		// identically on macOS, Linux, and Windows.
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// defaultSkip excludes paths we never want in a bundle: VCS metadata,
// the bundle output itself, and anything else that would bloat the
// archive without being part of the agent's source.
func defaultSkip(outPath string) func(rel string) bool {
	outName := filepath.Base(outPath)
	return func(rel string) bool {
		// Top-level VCS dirs.
		switch rel {
		case ".git", ".hg", ".svn":
			return true
		}
		if strings.HasPrefix(rel, ".git/") {
			return true
		}
		// The output file, in case it lives inside srcDir.
		if rel == outName {
			return true
		}
		return false
	}
}
