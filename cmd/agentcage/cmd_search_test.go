package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/store"
)

func TestSearchLocal_FiltersByRef(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	hash := buildStoredBundle(t, st)
	ref, err := reference.Parse("@me/researcher:0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tag(ref, hash); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	var hit bytes.Buffer
	if err := searchLocal(&hit, "research", false); err != nil {
		t.Fatalf("searchLocal: %v", err)
	}
	if !strings.Contains(hit.String(), "researcher") {
		t.Errorf("output = %q, want the matching ref", hit.String())
	}

	var miss bytes.Buffer
	if err := searchLocal(&miss, "nomatch", false); err != nil {
		t.Fatalf("searchLocal: %v", err)
	}
	if strings.Contains(miss.String(), "researcher") {
		t.Errorf("output = %q, want no match", miss.String())
	}
}

func TestPrintSearchResults_RendersEvalSignal(t *testing.T) {
	servers := []mcpregistry.Server{{
		Name:        "io.github.a/fs",
		Version:     "0.1",
		Description: "a filesystem agent",
		Meta: map[string]any{
			"io.agentcage/evals": map[string]any{"declared": true, "passed": 47.0, "failed": 3.0, "judge_score": 0.83},
		},
	}}
	var out bytes.Buffer
	printSearchResults(&out, servers)
	got := out.String()
	if !strings.Contains(got, "47/50") || !strings.Contains(got, "j0.83") {
		t.Errorf("output = %q, want the eval signal rendered", got)
	}
}
