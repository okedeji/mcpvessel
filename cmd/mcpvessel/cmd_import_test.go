package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/wrap"
)

func TestResolveSystemPrompt(t *testing.T) {
	// Inline prompt passes through.
	if got, err := resolveSystemPrompt("be terse", ""); err != nil || got != "be terse" {
		t.Errorf("inline = %q, %v; want the prompt", got, err)
	}
	// No prompt at all is fine (harness uses its internal prompt alone).
	if got, err := resolveSystemPrompt("", ""); err != nil || got != "" {
		t.Errorf("none = %q, %v; want empty", got, err)
	}
	// Both at once is refused.
	if _, err := resolveSystemPrompt("x", "y"); err == nil {
		t.Error("want an error when both --prompt and --prompt-file are given")
	}

	// A file preserves multi-line content, trimmed.
	dir := t.TempDir()
	file := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(file, []byte("\nYou are an SRE.\nEscalate P1s.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSystemPrompt("", file)
	if err != nil {
		t.Fatalf("prompt-file: %v", err)
	}
	if got != "You are an SRE.\nEscalate P1s." {
		t.Errorf("prompt-file = %q, want the multi-line prompt trimmed", got)
	}

	// An empty file is a mistake, not a silent no-prompt.
	empty := filepath.Join(dir, "empty.md")
	_ = os.WriteFile(empty, []byte("   \n"), 0o644)
	if _, err := resolveSystemPrompt("", empty); err == nil {
		t.Error("want an error for an empty --prompt-file")
	}
}

func TestResolveImportSource_DirectCoordinate(t *testing.T) {
	src, err := resolveImportSource(context.Background(), "npm:@modelcontextprotocol/server-filesystem@1.0")
	if err != nil {
		t.Fatalf("resolveImportSource: %v", err)
	}
	if src.Registry != wrap.NPM || src.Identifier != "@modelcontextprotocol/server-filesystem" || src.Version != "1.0" {
		t.Errorf("source = %+v, want the parsed npm coordinate", src)
	}
}

// stubRegistryServer serves one entry at /v0.1/servers so a reverse-DNS ref
// resolves without the real registry.
func stubRegistryServer(t *testing.T, entry map[string]any) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0.1/servers" {
			_ = json.NewEncoder(w).Encode(map[string]any{"servers": []map[string]any{{"server": entry}}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	t.Setenv(env.MCPRegistry, ts.URL)
}

func TestResolveImportSource_RegistryPackage(t *testing.T) {
	stubRegistryServer(t, map[string]any{
		"name": "io.github.mcp/fs",
		"packages": []map[string]any{{
			"registryType": "npm", "identifier": "@mcp/fs", "version": "2.0",
			"environmentVariables": []map[string]any{
				{"name": "ROOT", "isRequired": true},
				{"name": "TOKEN", "isSecret": true},
			},
		}},
	})
	src, err := resolveImportSource(context.Background(), "io.github.mcp/fs")
	if err != nil {
		t.Fatalf("resolveImportSource: %v", err)
	}
	if src.Registry != wrap.NPM || src.Identifier != "@mcp/fs" || src.Version != "2.0" {
		t.Errorf("source = %+v, want the npm package from the entry", src)
	}
	var sawSecret bool
	for _, e := range src.Env {
		if e.Name == "TOKEN" && e.Secret {
			sawSecret = true
		}
	}
	if !sawSecret {
		t.Errorf("env = %+v, want TOKEN mapped as a secret", src.Env)
	}
}

func TestResolveImportSource_RemoteOnlyRefused(t *testing.T) {
	stubRegistryServer(t, map[string]any{
		"name":    "io.github.mcp/hosted",
		"remotes": []map[string]any{{"type": "streamable-http", "url": "https://mcp.example.com"}},
	})
	_, err := resolveImportSource(context.Background(), "io.github.mcp/hosted")
	if err == nil || !strings.Contains(err.Error(), "remote MCP server") || !strings.Contains(err.Error(), "EGRESS") {
		t.Fatalf("err = %v, want a remote refusal pointing at EGRESS", err)
	}
}

func TestWriteGeneratedVesselfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fs")
	src := wrap.Source{Registry: wrap.NPM, Identifier: "@mcp/fs", Version: "2.0"}
	if err := writeGeneratedVesselfile(dir, src, false); err != nil {
		t.Fatalf("writeGeneratedVesselfile: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Vesselfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "RUN npm install -g @mcp/fs@2.0") {
		t.Errorf("Vesselfile = %q, want the npm install line", raw)
	}

	// A second import into the same dir must not clobber the first.
	if err := writeGeneratedVesselfile(dir, src, false); err == nil {
		t.Error("want an error rather than overwriting an existing Vesselfile")
	}
}

func TestDefaultImportDir(t *testing.T) {
	cases := map[string]string{
		"@modelcontextprotocol/server-filesystem": "server-filesystem",
		"ghcr.io/acme/mcp-slack":                  "mcp-slack",
		"mcp-server-fetch":                        "mcp-server-fetch",
	}
	for id, want := range cases {
		got := defaultImportDir(wrap.Source{Identifier: id})
		if filepath.Base(got) != want {
			t.Errorf("defaultImportDir(%q) = %q, want basename %q", id, got, want)
		}
	}
}

// A failed import removes only a directory it created itself; a pre-existing
// one may hold hand edits and stays.
func TestRemoveGenerated(t *testing.T) {
	base := t.TempDir()

	created := filepath.Join(base, "fresh")
	if err := os.MkdirAll(created, 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	removeGenerated(&out, created, true)
	if dirExists(created) {
		t.Error("created dir survived cleanup")
	}
	if !strings.Contains(out.String(), "retry starts fresh") {
		t.Errorf("cleanup note = %q, want the retry hint", out.String())
	}

	preexisting := filepath.Join(base, "mine")
	if err := os.MkdirAll(preexisting, 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	removeGenerated(&out, preexisting, false)
	if !dirExists(preexisting) {
		t.Error("pre-existing dir was deleted")
	}
	if out.String() != "" {
		t.Errorf("unexpected note for untouched dir: %q", out.String())
	}
}

// lineHas reports whether some line of s contains both substrings, so a check
// does not depend on tabwriter's column padding.
func lineHas(s, a, b string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, a) && strings.Contains(line, b) {
			return true
		}
	}
	return false
}

func TestPrintImportInputs_MarksSuppliedAndUsesSecretFlag(t *testing.T) {
	declared := []wrap.EnvVar{
		{Name: "GITHUB_PERSONAL_ACCESS_TOKEN", Secret: true, Required: true, Description: "GitHub token"},
		{Name: "BRAVE_API_KEY", Secret: true, Required: true, Description: "Brave key"},
	}
	var out strings.Builder
	printImportInputs(&out, declared,
		nil,
		map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "x"}, // one supplied, one not
	)
	got := out.String()

	if !lineHas(got, "GITHUB_PERSONAL_ACCESS_TOKEN", "supplied") {
		t.Errorf("supplied secret not marked supplied:\n%s", got)
	}
	if !lineHas(got, "BRAVE_API_KEY", "needed") {
		t.Errorf("missing secret not marked needed:\n%s", got)
	}
	// Secrets are name-only, so the hint must steer to --secret, never --env.
	if !strings.Contains(got, "--secret NAME") {
		t.Errorf("missing --secret hint:\n%s", got)
	}
	if strings.Contains(got, "--env") {
		t.Errorf("secret-only inputs must not suggest --env:\n%s", got)
	}
}

func TestPrintImportInputs_SilentWhenNoneDeclared(t *testing.T) {
	var out strings.Builder
	printImportInputs(&out, nil, nil, nil)
	if out.String() != "" {
		t.Errorf("expected no output, got %q", out.String())
	}
}

func TestWriteGeneratedVesselfile_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	src := wrap.Source{Registry: wrap.PyPI, Identifier: "mcp-server-time"}

	if err := writeGeneratedVesselfile(dir, src, false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// A second write without --force refuses and points at the real fix.
	err := writeGeneratedVesselfile(dir, src, false)
	if err == nil {
		t.Fatal("expected refusal on existing Vesselfile")
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "mcpvessel build") {
		t.Errorf("error should explain the fix, got: %v", err)
	}
	// With --force it overwrites instead.
	if err := writeGeneratedVesselfile(dir, src, true); err != nil {
		t.Errorf("force write should overwrite, got: %v", err)
	}
}
