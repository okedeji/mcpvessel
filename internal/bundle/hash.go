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

// hashFiles returns a sha256 over a canonical encoding of every regular file
// under root: `<relpath>\0<content>\0`, sorted by relative path. Including
// the path means a rename changes the hash even when content does not. skip
// is checked against the root-relative path.
func hashFiles(root string, skip func(rel string) bool) (string, error) {
	paths, err := walkFiles(root, skip)
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, rel := range paths {
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

// walkFiles returns the slash-separated relative paths of every regular file
// under root, filtered by skip.
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
		// Slash-normalized so the same tree hashes identically on every OS.
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

// defaultSkip excludes VCS metadata and the bundle output (plus its .tmp
// staging file) when they live inside srcDir.
func defaultSkip(outPath string) func(rel string) bool {
	outName := filepath.Base(outPath)
	return func(rel string) bool {
		switch rel {
		case ".git", ".hg", ".svn":
			return true
		}
		if strings.HasPrefix(rel, ".git/") {
			return true
		}
		if rel == outName || rel == outName+".tmp" {
			return true
		}
		return false
	}
}
