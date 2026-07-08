package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
)

func TestBuildAgent_RejectsNilBuildKit(t *testing.T) {
	err := BuildAgent(context.Background(), nil, BuildInput{
		Agentfile: &agentfile.Agentfile{From: "x", Entrypoint: "y"},
		SourceDir: t.TempDir(),
		ImageRef:  "foo:1",
	})
	if err == nil || !strings.Contains(err.Error(), "buildkit") {
		t.Errorf("expected nil-client error, got: %v", err)
	}
}

func TestBuildAgent_RejectsNilAgentfile(t *testing.T) {
	bk := &BuildKit{} // not dialed; BuildAgent should reject before touching it
	err := BuildAgent(context.Background(), bk, BuildInput{
		SourceDir: t.TempDir(),
		ImageRef:  "foo:1",
	})
	if err == nil || !strings.Contains(err.Error(), "agentfile") {
		t.Errorf("expected nil-agentfile error, got: %v", err)
	}
}

func TestBuildAgent_RejectsEmptySourceDir(t *testing.T) {
	bk := &BuildKit{}
	err := BuildAgent(context.Background(), bk, BuildInput{
		Agentfile: &agentfile.Agentfile{From: "x", Entrypoint: "y"},
		ImageRef:  "foo:1",
	})
	if err == nil || !strings.Contains(err.Error(), "source") {
		t.Errorf("expected empty-source error, got: %v", err)
	}
}

func TestBuildAgent_RejectsEmptyImageRef(t *testing.T) {
	bk := &BuildKit{}
	err := BuildAgent(context.Background(), bk, BuildInput{
		Agentfile: &agentfile.Agentfile{From: "x", Entrypoint: "y"},
		SourceDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "image ref") {
		t.Errorf("expected empty-image-ref error, got: %v", err)
	}
}

func TestWriteBuildContext_ProducesReadableFile(t *testing.T) {
	in := BuildInput{
		Agentfile: &agentfile.Agentfile{
			From:       "python:3.12-slim",
			Entrypoint: "python3 main.py",
		},
	}
	dir, cleanup, err := writeBuildContext(in)
	if err != nil {
		t.Fatalf("writeBuildContext: %v", err)
	}
	defer cleanup()

	content, err := os.ReadFile(filepath.Join(dir, "Agentfile"))
	if err != nil {
		t.Fatalf("read Agentfile: %v", err)
	}
	if !strings.Contains(string(content), "FROM python:3.12-slim") {
		t.Errorf("Agentfile missing FROM line:\n%s", content)
	}
}

func TestWriteBuildContext_CleanupRemovesDir(t *testing.T) {
	in := BuildInput{
		Agentfile: &agentfile.Agentfile{From: "x", Entrypoint: "y"},
	}
	dir, cleanup, err := writeBuildContext(in)
	if err != nil {
		t.Fatalf("writeBuildContext: %v", err)
	}
	cleanup()
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("cleanup left dir behind: %s (stat err=%v)", dir, statErr)
	}
}

func TestLabelsFromManifest_NilSafe(t *testing.T) {
	if got := labelsFromManifest(nil); got != nil {
		t.Errorf("nil manifest should yield nil labels, got %v", got)
	}
}

func TestLabelsFromManifest_PopulatesProvenance(t *testing.T) {
	m := &bundle.Manifest{
		SpecVersion: "1",
		FilesHash:   "sha256:abc",
		BuiltWith:   "agentcage 0.1.0",
		BuiltAt:     time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC),
	}
	got := labelsFromManifest(m)

	wantSubset := map[string]string{
		"io.agentcage.spec_version": "1",
		"io.agentcage.files_hash":   "sha256:abc",
		"io.agentcage.built_with":   "agentcage 0.1.0",
		"io.agentcage.built_at":     "2026-06-07T00:00:00Z",
	}
	for k, v := range wantSubset {
		if got[k] != v {
			t.Errorf("label %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestRewriteAgentcageDisplay(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"[internal] load build definition from Dockerfile", "[internal] load build definition from Agentfile"},
		{"[internal] load .dockerignore", "[internal] load .agentignore"},
		{"transferring dockerfile: 493B done", "transferring agentfile: 493B done"},
		{"[internal] load metadata for docker.io/library/python:3.12-slim", "[internal] load metadata for python:3.12-slim"},
		{"FROM docker.io/library/node:20-slim", "FROM node:20-slim"},
		{"docker.io/myorg/custom:1.0", "myorg/custom:1.0"},
		{"[2/4] WORKDIR /agent", "[2/4] WORKDIR /agent"},
		{"exporting to image", "exporting to image"},
	}
	for _, tc := range cases {
		if got := rewriteAgentcageDisplay(tc.in); got != tc.want {
			t.Errorf("rewriteAgentcageDisplay(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLabelsFromManifest_OmitsBuiltAtWhenZero(t *testing.T) {
	m := &bundle.Manifest{
		SpecVersion: "1",
		FilesHash:   "sha256:abc",
		BuiltWith:   "agentcage 0.1.0",
	}
	got := labelsFromManifest(m)
	if _, ok := got["io.agentcage.built_at"]; ok {
		t.Errorf("zero BuiltAt should not produce a built_at label")
	}
}
