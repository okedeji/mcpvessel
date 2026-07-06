package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
)

func TestParseArgPairs_Empty(t *testing.T) {
	got, err := parseArgPairs(nil)
	if err != nil {
		t.Fatalf("parseArgPairs(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestParseArgPairs_HappyPath(t *testing.T) {
	got, err := parseArgPairs([]string{"name=World", "depth=deep"})
	if err != nil {
		t.Fatalf("parseArgPairs: %v", err)
	}
	want := map[string]any{
		"name":  "World",
		"depth": "deep",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseArgPairs = %v, want %v", got, want)
	}
}

func TestParseArgPairs_AllowsEqualsInValue(t *testing.T) {
	got, err := parseArgPairs([]string{"expr=1+2=3"})
	if err != nil {
		t.Fatalf("parseArgPairs: %v", err)
	}
	if got["expr"] != "1+2=3" {
		t.Errorf("expr = %q, want %q (only the first = is the separator)", got["expr"], "1+2=3")
	}
}

func TestParseArgPairs_RejectsNoEquals(t *testing.T) {
	_, err := parseArgPairs([]string{"justakey"})
	if err == nil {
		t.Fatalf("expected error for malformed arg")
	}
	if !strings.Contains(err.Error(), "justakey") {
		t.Errorf("error %q should name the offending arg", err.Error())
	}
}

func TestParseArgPairs_RejectsEmptyKey(t *testing.T) {
	_, err := parseArgPairs([]string{"=value"})
	if err == nil {
		t.Fatalf("expected error for empty key")
	}
}

func TestAssertToolIsPublic_AcceptsMain(t *testing.T) {
	m := &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Main: "respond"}}
	if err := assertToolIsPublic(m, "respond"); err != nil {
		t.Errorf("main tool should be public, got: %v", err)
	}
}

func TestAssertToolIsPublic_AcceptsExposed(t *testing.T) {
	m := &bundle.Manifest{Agentfile: bundle.AgentfileSpec{
		Main:   "respond",
		Expose: []string{"fetch_paper", "cite_count"},
	}}
	for _, name := range []string{"fetch_paper", "cite_count"} {
		if err := assertToolIsPublic(m, name); err != nil {
			t.Errorf("exposed tool %q should be public, got: %v", name, err)
		}
	}
}

func TestAssertToolIsPublic_RejectsPrivate(t *testing.T) {
	m := &bundle.Manifest{Agentfile: bundle.AgentfileSpec{
		Main:   "respond",
		Expose: []string{"fetch_paper"},
	}}
	err := assertToolIsPublic(m, "parse_doi")
	if err == nil {
		t.Fatalf("expected error for private tool")
	}
	for _, want := range []string{"parse_doi", "respond", "fetch_paper"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestAssertToolIsPublic_ToolCollectionWithExpose(t *testing.T) {
	m := &bundle.Manifest{Agentfile: bundle.AgentfileSpec{
		Expose: []string{"search", "news"},
	}}
	if err := assertToolIsPublic(m, "search"); err != nil {
		t.Errorf("search should be public in a tool collection, got: %v", err)
	}
	err := assertToolIsPublic(m, "private_helper")
	if err == nil {
		t.Fatalf("expected error for non-exposed tool")
	}
}

func TestAssertToolIsPublic_UsesCatalogWhenPresent(t *testing.T) {
	// EXPOSE * leaves the raw directive as "*", but the catalog carries the
	// expanded per-tool visibility, and the check must read the catalog.
	m := &bundle.Manifest{
		Agentfile: bundle.AgentfileSpec{Expose: []string{"*"}},
		Tools: []bundle.Tool{
			{Name: "echo", Visibility: bundle.VisibilityPublic},
			{Name: "debug_env", Visibility: bundle.VisibilityPrivate},
		},
	}
	if err := assertToolIsPublic(m, "echo"); err != nil {
		t.Errorf("echo is public in the catalog, got: %v", err)
	}
	err := assertToolIsPublic(m, "debug_env")
	if err == nil || !strings.Contains(err.Error(), "echo") {
		t.Fatalf("err = %v, want a rejection listing echo as the public tool", err)
	}
}

func TestAssertToolIsPublic_NoPublicSurfaceAtAll(t *testing.T) {
	m := &bundle.Manifest{Agentfile: bundle.AgentfileSpec{}}
	err := assertToolIsPublic(m, "anything")
	if err == nil {
		t.Fatalf("expected error for bundle with no public surface")
	}
	if !strings.Contains(err.Error(), "no public tools") {
		t.Errorf("error %q should mention empty public surface", err.Error())
	}
}

func TestCallCmd_RequiresBundleAndTool(t *testing.T) {
	cmd := newCallCmd()
	cmd.SetArgs([]string{"a.agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected missing-arg error")
	}
}
