package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
)

func TestPrintManifest_RendersCatalogAndUses(t *testing.T) {
	m := &bundle.Manifest{
		SpecVersion: "0.1",
		FilesHash:   "sha256:abc",
		Agentfile: bundle.AgentfileSpec{
			From:       "python:3.12-slim",
			Entrypoint: "python3 agent.py",
			Run:        []string{"pip install -r requirements.txt"},
			Model:      "anthropic/claude-3.5",
			Main:       "respond",
			Expose:     []string{"fetch_paper"},
			Budget:     100000,
			Network:    "allow:api.example.com",
			Secrets:    []string{"anthropic_api_key"},
			Env:        map[string]string{"LOG_LEVEL": "info"},
			Meta:       map[string]string{"license": "MIT"},
			Eval:       "tests/eval.yaml",
			Uses: []bundle.UseSpec{
				{Ref: "@anthropic/web-search", Version: "1.2.0", Digest: "sha256:web", Public: true, Deny: []string{"deep_crawl"}},
			},
		},
		Tools: []bundle.Tool{
			{Name: "respond", Visibility: bundle.VisibilityMain, Description: "Research a topic."},
			{Name: "fetch_paper", Visibility: bundle.VisibilityPublic},
			{Name: "parse_doi", Visibility: bundle.VisibilityPrivate, Description: "Normalize a DOI."},
		},
	}

	var buf bytes.Buffer
	printManifest(&buf, "researcher.agent", m)
	out := buf.String()

	for _, want := range []string{
		"researcher.agent",
		"python:3.12-slim",
		"pip install -r requirements.txt",
		"BUDGET", "100000",
		"allow:api.example.com",
		"anthropic_api_key",
		"LOG_LEVEL=info",
		"license MIT",
		"tests/eval.yaml",
		"respond", "main", "Research a topic.",
		"fetch_paper", "public",
		"parse_doi", "private", "Normalize a DOI.",
		"@anthropic/web-search:1.2.0", "[public]", "sha256:web", "DENY deep_crawl",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

func TestSchemaSignature(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"no schema", nil, ""},
		{"no params", map[string]any{"type": "object", "properties": map[string]any{}}, "()"},
		{
			"one required param",
			map[string]any{
				"properties": map[string]any{"message": map[string]any{"type": "string"}},
				"required":   []any{"message"},
			},
			"(message: string)",
		},
		{
			"required and optional, sorted",
			map[string]any{
				"properties": map[string]any{
					"message": map[string]any{"type": "string"},
					"depth":   map[string]any{"type": "string"},
				},
				"required": []any{"message"},
			},
			"(depth?: string, message: string)",
		},
		{
			"param without a type",
			map[string]any{
				"properties": map[string]any{"thing": map[string]any{}},
				"required":   []any{"thing"},
			},
			"(thing: any)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaSignature(tc.schema); got != tc.want {
				t.Errorf("schemaSignature = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveInspectTarget_LocalFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.agent")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, display, err := resolveInspectTarget(context.Background(), p)
	if err != nil {
		t.Fatalf("resolveInspectTarget: %v", err)
	}
	if path != p || display != p {
		t.Errorf("got (%q, %q), want both %q", path, display, p)
	}
}

func TestResolveInspectTarget_BogusArgErrors(t *testing.T) {
	// Not an existing file and not a parseable reference.
	if _, _, err := resolveInspectTarget(context.Background(), "not a ref and not a file"); err == nil {
		t.Fatal("expected an error for an arg that is neither a file nor a ref")
	}
}

func TestResolveInspectTarget_RefWithoutVersionErrors(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	// A valid ref shape but no tag/digest: nothing to pull.
	if _, _, err := resolveInspectTarget(context.Background(), "@anthropic/web-search"); err == nil {
		t.Fatal("expected an error for a ref with no version")
	}
}

func TestInspectCmd_MissingBundleErrors(t *testing.T) {
	cmd := newInspectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"/no/such/bundle.agent"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error inspecting a missing bundle")
	}
}
