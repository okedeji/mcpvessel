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
	"time"
)

// minimalSource writes a small but valid Agentfile + agent.py into dir.
func minimalSource(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "Agentfile"), `BASE python:3.12-slim
BUILD pip install --no-cache-dir agentcage-sdk
ENTRYPOINT python3 agent.py
MODEL anthropic/claude-3.5
META description "test agent"
`)
	writeFile(t, filepath.Join(dir, "agent.py"), "print('hello')\n")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestBuild_HappyPath(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(t.TempDir(), "agent.agent")
	minimalSource(t, src)

	// Pin BuiltAt so the manifest is deterministic for assertions.
	prev := nowFunc
	t.Cleanup(func() { nowFunc = prev })
	nowFunc = func() time.Time { return time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC) }

	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	manifest, files := extract(t, out)

	if manifest.SpecVersion != "0.1" {
		t.Errorf("SpecVersion = %q, want 0.1", manifest.SpecVersion)
	}
	if !strings.HasPrefix(manifest.FilesHash, "sha256:") {
		t.Errorf("FilesHash = %q, want sha256: prefix", manifest.FilesHash)
	}
	if manifest.Agentfile.Base != "python:3.12-slim" {
		t.Errorf("Agentfile.Base = %q", manifest.Agentfile.Base)
	}
	if manifest.Agentfile.Model != "anthropic/claude-3.5" {
		t.Errorf("Agentfile.Model = %q, want anthropic/claude-3.5", manifest.Agentfile.Model)
	}
	want := map[string]string{
		"files/Agentfile": "BASE python:3.12-slim\nBUILD pip install --no-cache-dir agentcage-sdk\nENTRYPOINT python3 agent.py\nMODEL anthropic/claude-3.5\nMETA description \"test agent\"\n",
		"files/agent.py":  "print('hello')\n",
	}
	if len(files) != len(want) {
		t.Errorf("file count = %d, want %d (got %v)", len(files), len(want), keysOf(files))
	}
	for path, body := range want {
		if got, ok := files[path]; !ok {
			t.Errorf("missing %s in bundle", path)
		} else if got != body {
			t.Errorf("%s body mismatch:\n got %q\nwant %q", path, got, body)
		}
	}
}

func TestBuild_HashIsDeterministic(t *testing.T) {
	src := t.TempDir()
	minimalSource(t, src)

	out1 := filepath.Join(t.TempDir(), "a.agent")
	out2 := filepath.Join(t.TempDir(), "b.agent")
	if err := Build(src, out1); err != nil {
		t.Fatalf("Build 1: %v", err)
	}
	if err := Build(src, out2); err != nil {
		t.Fatalf("Build 2: %v", err)
	}
	m1, _ := extract(t, out1)
	m2, _ := extract(t, out2)
	if m1.FilesHash != m2.FilesHash {
		t.Errorf("files hash drifted across builds: %q vs %q", m1.FilesHash, m2.FilesHash)
	}
}

func TestBuild_HashChangesWhenContentChanges(t *testing.T) {
	src := t.TempDir()
	minimalSource(t, src)

	out1 := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out1); err != nil {
		t.Fatalf("Build before edit: %v", err)
	}
	m1, _ := extract(t, out1)

	// Change one file's content.
	writeFile(t, filepath.Join(src, "agent.py"), "print('different')\n")

	out2 := filepath.Join(t.TempDir(), "b.agent")
	if err := Build(src, out2); err != nil {
		t.Fatalf("Build after edit: %v", err)
	}
	m2, _ := extract(t, out2)
	if m1.FilesHash == m2.FilesHash {
		t.Errorf("files hash did not change after a source edit (%q)", m1.FilesHash)
	}
}

func TestBuild_SkipsVCSDir(t *testing.T) {
	src := t.TempDir()
	minimalSource(t, src)
	writeFile(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(src, ".git", "config"), "[core]\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, files := extract(t, out)
	for path := range files {
		if strings.HasPrefix(path, "files/.git/") {
			t.Errorf("bundle contains VCS metadata: %s", path)
		}
	}
}

func TestBuild_MissingAgentfile(t *testing.T) {
	src := t.TempDir()
	// No Agentfile written.
	out := filepath.Join(t.TempDir(), "a.agent")
	err := Build(src, out)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Agentfile not found") {
		t.Errorf("error = %q, want 'Agentfile not found'", err.Error())
	}
}

func TestBuild_InvalidAgentfile(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `ENTRYPOINT python3 agent.py
`)
	out := filepath.Join(t.TempDir(), "a.agent")
	err := Build(src, out)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	// Half-written output must not remain.
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("output file exists after failed build: %s", out)
	}
}

// extract opens a .agent file and returns the manifest plus a map of
// archive-relative paths to file contents for every non-manifest entry.
func extract(t *testing.T, path string) (*Manifest, map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)

	var manifest *Manifest
	files := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		if hdr.Name == "manifest.json" {
			manifest = &Manifest{}
			if err := json.Unmarshal(buf.Bytes(), manifest); err != nil {
				t.Fatalf("decoding manifest: %v", err)
			}
			continue
		}
		files[hdr.Name] = buf.String()
	}
	if manifest == nil {
		t.Fatalf("manifest.json missing from bundle")
	}
	return manifest, files
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
