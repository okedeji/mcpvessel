package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/agentfile"
)

// minimalSource writes a small but valid Agentfile + agent.py into dir.
func minimalSource(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "Agentfile"), `FROM python:3.12-slim
RUN pip install --no-cache-dir mcp
MODEL anthropic/claude-3.5
MAIN respond
EXPOSE fetch_paper
META description "test agent"
ENTRYPOINT python3 agent.py
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
	if manifest.Agentfile.From != "python:3.12-slim" {
		t.Errorf("Agentfile.From = %q", manifest.Agentfile.From)
	}
	if manifest.Agentfile.Model != "anthropic/claude-3.5" {
		t.Errorf("Agentfile.Model = %q, want anthropic/claude-3.5", manifest.Agentfile.Model)
	}
	if manifest.Agentfile.Main != "respond" {
		t.Errorf("Agentfile.Main = %q, want %q", manifest.Agentfile.Main, "respond")
	}
	if len(manifest.Agentfile.Expose) != 1 || manifest.Agentfile.Expose[0] != "fetch_paper" {
		t.Errorf("Agentfile.Expose = %v, want [fetch_paper]", manifest.Agentfile.Expose)
	}

	// Catalog mirrors MAIN + EXPOSE in M1. Private tools and descriptions
	// arrive in M2 once the build introspects the running agent.
	wantTools := []Tool{
		{Name: "respond", Visibility: VisibilityMain},
		{Name: "fetch_paper", Visibility: VisibilityPublic},
	}
	if !reflect.DeepEqual(manifest.Tools, wantTools) {
		t.Errorf("Tools = %+v, want %+v", manifest.Tools, wantTools)
	}
	want := map[string]string{
		"files/Agentfile": "FROM python:3.12-slim\nRUN pip install --no-cache-dir mcp\nMODEL anthropic/claude-3.5\nMAIN respond\nEXPOSE fetch_paper\nMETA description \"test agent\"\nENTRYPOINT python3 agent.py\n",
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

func TestBuild_UsesDenyRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
RUN pip install --no-cache-dir mcp
MAIN respond
USES @anthropic/web-search:1.2.0 DENY deep_crawl
USES PUBLIC @user/billing:0.5.0 DENY charge_card,refund
USES @vendor/safe:1.0.0
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	manifest, _ := extract(t, out)

	want := []UseSpec{
		{Ref: "@anthropic/web-search", Version: "1.2.0", Deny: []string{"deep_crawl"}},
		{Ref: "@user/billing", Version: "0.5.0", Public: true, Deny: []string{"charge_card", "refund"}},
		{Ref: "@vendor/safe", Version: "1.0.0"},
	}
	if !reflect.DeepEqual(manifest.Agentfile.Uses, want) {
		t.Errorf("Uses = %+v, want %+v", manifest.Agentfile.Uses, want)
	}
}

func TestBuild_UsesResolverLocksDigests(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
MAIN respond
USES @anthropic/web-search:1.2.0
USES @user/pdf:0.4.0
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")

	digests := map[string]string{
		"@anthropic/web-search": "sha256:aaaa",
		"@user/pdf":             "sha256:bbbb",
	}
	out := filepath.Join(t.TempDir(), "a.agent")
	err := Build(src, out, WithUsesResolver(func(u agentfile.Use) (string, error) {
		return digests[u.Ref], nil
	}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	manifest, _ := extract(t, out)
	for _, u := range manifest.Agentfile.Uses {
		if u.Digest != digests[u.Ref] {
			t.Errorf("Uses[%s].Digest = %q, want %q", u.Ref, u.Digest, digests[u.Ref])
		}
	}
}

func TestBuild_UsesResolverErrorFailsBuild(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
MAIN respond
USES @anthropic/web-search:1.2.0
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	err := Build(src, out, WithUsesResolver(func(u agentfile.Use) (string, error) {
		return "", os.ErrNotExist
	}))
	if err == nil {
		t.Fatal("expected build to fail when a USES digest cannot resolve")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("bundle written despite resolver failure: %s", out)
	}
}

func TestBuild_IntrospectionEnrichesCatalog(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
MAIN respond
EXPOSE fetch_paper
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")

	introspected := []IntrospectedTool{
		{Name: "respond", Description: "Reason about a prompt.", Schema: map[string]any{"type": "object"}},
		{Name: "fetch_paper", Description: "Fetch a paper."},
		// served by the agent but not declared: discovered as private.
		{Name: "parse_doi", Description: "Normalize a DOI."},
	}
	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out, WithIntrospectedTools(introspected)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	manifest, _ := extract(t, out)

	got := map[string]Tool{}
	for _, tool := range manifest.Tools {
		got[tool.Name] = tool
	}
	wantVis := map[string]Visibility{
		"respond":     VisibilityMain,
		"fetch_paper": VisibilityPublic,
		"parse_doi":   VisibilityPrivate,
	}
	for name, vis := range wantVis {
		if got[name].Visibility != vis {
			t.Errorf("%s visibility = %q, want %q", name, got[name].Visibility, vis)
		}
	}
	if got["respond"].Description != "Reason about a prompt." {
		t.Errorf("respond description not carried: %q", got["respond"].Description)
	}
	if got["respond"].Schema["type"] != "object" {
		t.Errorf("respond schema not carried: %+v", got["respond"].Schema)
	}
}

func TestBuild_IntrospectionRejectsDeclaredToolTheAgentDoesNotServe(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
MAIN respond
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")

	// The agent serves "chat", but MAIN names "respond": a typo the build
	// must catch.
	out := filepath.Join(t.TempDir(), "a.agent")
	err := Build(src, out, WithIntrospectedTools([]IntrospectedTool{{Name: "chat"}}))
	if err == nil {
		t.Fatal("expected build to fail when MAIN names a tool the agent does not serve")
	}
	if !strings.Contains(err.Error(), "respond") {
		t.Errorf("error %q should name the missing MAIN tool", err.Error())
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("bundle written despite validation failure: %s", out)
	}
}

func TestBuild_CatalogOmittedForToolCollectionWithoutMainOrExpose(t *testing.T) {
	// Pathological case: a bundle that ships an MCP server but
	// declares neither MAIN nor EXPOSE. The build still succeeds; the
	// catalog is empty (omitempty drops it from JSON). The bundle is
	// not callable via run or call, but that is the operator's problem,
	// not the build's.
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	manifest, _ := extract(t, out)
	if manifest.Tools != nil {
		t.Errorf("Tools = %+v, want nil for bundle with no MAIN or EXPOSE", manifest.Tools)
	}
}

func TestBuild_ExcludesOwnTempFileWhenBuildingIntoSourceDir(t *testing.T) {
	// Building with the output inside the source directory must not
	// package the build's own .tmp staging file. The hash walk runs
	// before the temp exists; the tar walk runs after, so the skip filter
	// has to exclude it or the archive disagrees with files_hash.
	src := t.TempDir()
	minimalSource(t, src)
	out := filepath.Join(src, "out.agent")

	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, files := extract(t, out)
	for path := range files {
		if strings.HasSuffix(path, ".tmp") {
			t.Errorf("bundle leaked a temp file: %s", path)
		}
		if strings.HasSuffix(path, "out.agent") {
			t.Errorf("bundle contains its own output: %s", path)
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
