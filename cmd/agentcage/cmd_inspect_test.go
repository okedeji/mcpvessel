package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

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
			Budget:     5_000_000,
			Resources:  &bundle.ResourcesSpec{CPUs: "2", Mem: "2g", Pids: 1024},
			Egress:     "allow:api.example.com",
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
		"BUDGET", "$5.00",
		"RESOURCES", "cpu=2 mem=2g pids=1024",
		"EGRESS", "allow:api.example.com",
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

func TestPrintManifest_EvalStatus(t *testing.T) {
	base := func() *bundle.Manifest {
		return &bundle.Manifest{
			SpecVersion: "0.1",
			FilesHash:   "sha256:abc",
			Agentfile:   bundle.AgentfileSpec{From: "x", Entrypoint: "y", Main: "respond", Eval: "tests/eval.yaml"},
		}
	}

	t.Run("declared but never run", func(t *testing.T) {
		m := base()
		m.Evals = &bundle.Evals{Declared: true}
		var buf bytes.Buffer
		printManifest(&buf, "a.agent", m)
		out := buf.String()
		if !strings.Contains(out, "Evals:") || !strings.Contains(out, "declared, never run") {
			t.Errorf("missing never-run status:\n%s", out)
		}
	})

	t.Run("stamped", func(t *testing.T) {
		m := base()
		at := time.Date(2026, 7, 4, 10, 12, 0, 0, time.UTC)
		passed, failed := 4, 1
		score := 0.83
		m.Evals = &bundle.Evals{Declared: true, Passed: &passed, Failed: &failed, JudgeScore: &score, LastRunAt: &at}
		var buf bytes.Buffer
		printManifest(&buf, "a.agent", m)
		out := buf.String()
		for _, want := range []string{"4 passed, 1 failed", "judge 0.83", "last run 2026-07-04T10:12:00Z"} {
			if !strings.Contains(out, want) {
				t.Errorf("stamped status missing %q:\n%s", want, out)
			}
		}
	})
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
