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

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/wrap"
)

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

func TestWriteGeneratedAgentfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fs")
	src := wrap.Source{Registry: wrap.NPM, Identifier: "@mcp/fs", Version: "2.0"}
	if err := writeGeneratedAgentfile(dir, src); err != nil {
		t.Fatalf("writeGeneratedAgentfile: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Agentfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "RUN npm install -g @mcp/fs@2.0") {
		t.Errorf("Agentfile = %q, want the npm install line", raw)
	}

	// A second import into the same dir must not clobber the first.
	if err := writeGeneratedAgentfile(dir, src); err == nil {
		t.Error("want an error rather than overwriting an existing Agentfile")
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
