package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtract_RoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
RUN pip install --no-cache-dir mcp
MODEL anthropic/claude-3.5
META description "test agent"
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('hello')\n")
	writeFile(t, filepath.Join(src, "nested", "helper.py"), "# helper\n")

	out := filepath.Join(t.TempDir(), "agent.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	dest := t.TempDir()
	manifest, err := Extract(out, dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if manifest.Agentfile.From != "python:3.12-slim" {
		t.Errorf("Manifest.From = %q", manifest.Agentfile.From)
	}
	if !strings.HasPrefix(manifest.FilesHash, "sha256:") {
		t.Errorf("FilesHash = %q", manifest.FilesHash)
	}

	for path, want := range map[string]string{
		"Agentfile":        "FROM python:3.12-slim\nRUN pip install --no-cache-dir mcp\nMODEL anthropic/claude-3.5\nMETA description \"test agent\"\nENTRYPOINT python3 agent.py\n",
		"agent.py":         "print('hello')\n",
		"nested/helper.py": "# helper\n",
	} {
		body, err := os.ReadFile(filepath.Join(dest, path))
		if err != nil {
			t.Errorf("missing %s: %v", path, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s body = %q, want %q", path, string(body), want)
		}
	}
}

func TestReadSourceFile_RoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), "FROM x\nENTRYPOINT y\n")
	writeFile(t, filepath.Join(src, "tests", "eval.yaml"), "version: 0.1\ncases: []\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	body, err := ReadSourceFile(out, "tests/eval.yaml")
	if err != nil {
		t.Fatalf("ReadSourceFile: %v", err)
	}
	if string(body) != "version: 0.1\ncases: []\n" {
		t.Errorf("body = %q", string(body))
	}
}

func TestReadSourceFile_Missing(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), "FROM x\nENTRYPOINT y\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	_, err := ReadSourceFile(out, "tests/eval.yaml")
	if err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Errorf("expected a missing-entry error, got %v", err)
	}
}

func TestReadSourceFile_RefusesEscape(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), "FROM x\nENTRYPOINT y\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A traversal path cleans to a files/ entry that cannot exist, so the
	// read fails closed.
	if _, err := ReadSourceFile(out, "../../etc/passwd"); err == nil {
		t.Error("expected an error for a path escaping the source root")
	}
}

func TestExtract_MissingBundle(t *testing.T) {
	_, err := Extract(filepath.Join(t.TempDir(), "nope.agent"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "open bundle") {
		t.Errorf("expected open error, got %v", err)
	}
}

func TestExtract_NotAGzip(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "bogus.agent")
	writeFile(t, junk, "this is not a gzip stream\n")
	_, err := Extract(junk, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "gunzip") {
		t.Errorf("expected gunzip error, got %v", err)
	}
}

func TestExtract_RefusesPathTraversal(t *testing.T) {
	// Build's own writer cannot produce an escaping entry name, so the tar
	// is crafted manually.
	bundlePath := filepath.Join(t.TempDir(), "evil.agent")
	mustWriteEvilBundle(t, bundlePath)

	_, err := Extract(bundlePath, t.TempDir())
	if err == nil {
		t.Fatalf("expected path-traversal error")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error %q should name the traversal", err.Error())
	}
}

func TestExtract_RejectsTamperedFiles(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), "FROM x\nENTRYPOINT y\n")
	writeFile(t, filepath.Join(src, "agent.py"), "print('original')\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// One source file rewritten, original manifest (and files_hash) kept:
	// the shape of a tampered local bundle.
	tampered := filepath.Join(t.TempDir(), "tampered.agent")
	repackReplacing(t, out, tampered, "files/agent.py", []byte("print('tampered')\n"))

	_, err := Extract(tampered, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "integrity check") {
		t.Fatalf("expected an integrity-check error, got %v", err)
	}
}

// repackReplacing copies a bundle, swapping one tar entry's body and leaving
// every other entry untouched.
func repackReplacing(t *testing.T, in, out, name string, body []byte) {
	t.Helper()
	inF, err := os.Open(in)
	if err != nil {
		t.Fatalf("open %s: %v", in, err)
	}
	defer func() { _ = inF.Close() }()
	gz, err := gzip.NewReader(inF)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	tr := tar.NewReader(gz)

	outF, err := os.Create(out)
	if err != nil {
		t.Fatalf("create %s: %v", out, err)
	}
	defer func() { _ = outF.Close() }()
	gw := gzip.NewWriter(outF)
	defer func() { _ = gw.Close() }()
	tw := tar.NewWriter(gw)
	defer func() { _ = tw.Close() }()

	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		content, _ := io.ReadAll(tr)
		if hdr.Name == name {
			content = body
			hdr.Size = int64(len(body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
}

// mustWriteEvilBundle writes a valid manifest plus one files/ entry whose
// relative path climbs above the destination directory.
func mustWriteEvilBundle(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create evil bundle: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	// Minimal valid manifest first so the loop reaches the evil entry.
	manifestBody, err := json.Marshal(&Manifest{
		SpecVersion: "0.1",
		Agentfile:   AgentfileSpec{From: "x", Entrypoint: "y"},
		FilesHash:   "sha256:deadbeef",
	})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "manifest.json",
		Mode: 0o644,
		Size: int64(len(manifestBody)),
	}); err != nil {
		t.Fatalf("write manifest header: %v", err)
	}
	if _, err := tw.Write(manifestBody); err != nil {
		t.Fatalf("write manifest body: %v", err)
	}

	body := []byte("pwned\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "files/../../escaped.txt",
		Mode: 0o644,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("write evil header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write evil body: %v", err)
	}
}
