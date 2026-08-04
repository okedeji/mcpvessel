package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shrinkExtractLimits drops the caps for one test, so a bomb can be a few
// kilobytes instead of half a gigabyte.
func shrinkExtractLimits(t *testing.T, limit int64, label string) {
	t.Helper()
	oldMax, oldFile, oldLabel := maxExtractedBytes, maxFileBytes, extractLimitLabel
	maxExtractedBytes, maxFileBytes, extractLimitLabel = limit, limit, label
	t.Cleanup(func() {
		maxExtractedBytes, maxFileBytes, extractLimitLabel = oldMax, oldFile, oldLabel
	})
}

// writeBomb builds an .agent whose files/ entries expand to size bytes of
// zeros. Gzip compresses those to almost nothing, which is the whole trick: the
// file on disk gives no hint of what it unpacks to. Split across entries so the
// running total, not just the per-file cap, is what has to catch it.
func writeBomb(t *testing.T, entries int, each int64) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	manifest, err := json.Marshal(Manifest{})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestFilename, Mode: 0o644, Size: int64(len(manifest))}); err != nil {
		t.Fatalf("manifest header: %v", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatalf("manifest body: %v", err)
	}

	for i := range entries {
		name := filesPrefix + "big" + string(rune('a'+i)) + ".bin"
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: each}); err != nil {
			t.Fatalf("entry header: %v", err)
		}
		if _, err := io.CopyN(tw, zeros{}, each); err != nil {
			t.Fatalf("entry body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	path := filepath.Join(t.TempDir(), "bomb.agent")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// A single entry larger than the cap is refused on that entry alone.
func TestExtract_RefusesAnOversizedEntry(t *testing.T) {
	shrinkExtractLimits(t, 64<<10, "64 KiB")
	bundlePath := writeBomb(t, 1, 128<<10)

	_, err := Extract(bundlePath, t.TempDir())
	if err == nil {
		t.Fatal("Extract accepted an entry past the per-file limit")
	}
	if !strings.Contains(err.Error(), extractLimitLabel) {
		t.Fatalf("Extract error = %v, want it to name the %s limit", err, extractLimitLabel)
	}
}

// Entries that each fit must still be refused once their total does not, or a
// bomb just splits itself up.
func TestExtract_RefusesManyEntriesThatTotalPastTheLimit(t *testing.T) {
	shrinkExtractLimits(t, 64<<10, "64 KiB")
	bundlePath := writeBomb(t, 4, 32<<10)

	_, err := Extract(bundlePath, t.TempDir())
	if err == nil {
		t.Fatal("Extract accepted a bundle whose entries total past the limit")
	}
	if !strings.Contains(err.Error(), extractLimitLabel) {
		t.Fatalf("Extract error = %v, want it to name the %s limit", err, extractLimitLabel)
	}
}

// The gzip file is tiny, so a cap on the archive rather than on what it expands
// to would not have caught either case.
func TestExtract_TheBombIsSmallOnDisk(t *testing.T) {
	bundlePath := writeBomb(t, 4, 32<<10)
	info, err := os.Stat(bundlePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 4<<10 {
		t.Fatalf("bomb is %d bytes on disk; the test no longer demonstrates the gap", info.Size())
	}
}

// A bundle inside the limits still extracts, so the cap did not just break
// every bundle.
func TestExtract_AcceptsAnOrdinaryBundle(t *testing.T) {
	shrinkExtractLimits(t, 64<<10, "64 KiB")
	bundlePath := writeBomb(t, 1, 1<<10)

	if _, err := Extract(bundlePath, t.TempDir()); err != nil {
		t.Fatalf("Extract refused a bundle within the limit: %v", err)
	}
}
