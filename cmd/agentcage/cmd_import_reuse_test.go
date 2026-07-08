package main

import (
	"testing"

	"github.com/okedeji/agentcage/internal/wrap"
)

func TestSplitAgentTag(t *testing.T) {
	prefix, version, err := splitAgentTag("@me/assistant:0.1")
	if err != nil || prefix != "@me/" || version != "0.1" {
		t.Fatalf("splitAgentTag = %q %q %v, want @me/ 0.1", prefix, version, err)
	}
	if _, _, err := splitAgentTag("@me/assistant"); err == nil {
		t.Error("want an error for a tag with no version")
	}
	if _, _, err := splitAgentTag("assistant:0.1"); err == nil {
		t.Error("want an error for a tag with no namespace")
	}
}

func TestToolSlugAndUnique(t *testing.T) {
	cases := map[string]string{
		"@modelcontextprotocol/server-github": "server-github",
		"mcp-server-time":                     "mcp-server-time",
		"ghcr.io/acme/mcp-slack":              "mcp-slack",
	}
	for id, want := range cases {
		if got := toolSlug(wrap.Source{Identifier: id}); got != want {
			t.Errorf("toolSlug(%q) = %q, want %q", id, got, want)
		}
	}

	used := map[string]bool{}
	a := uniqueSlug("srv", used)
	b := uniqueSlug("srv", used)
	if a != "srv" || b != "srv-2" {
		t.Errorf("uniqueSlug collision = %q %q, want srv srv-2", a, b)
	}
}

func TestParseToolArg(t *testing.T) {
	src, launch := parseToolArg("oci:ghcr.io/acme/mcp-slack:1.2 -- mcp-slack --stdio")
	if src != "oci:ghcr.io/acme/mcp-slack:1.2" || len(launch) != 2 || launch[0] != "mcp-slack" || launch[1] != "--stdio" {
		t.Errorf("parseToolArg = %q %v, want the source split from its launch", src, launch)
	}
	if src, launch := parseToolArg("npm:@scope/srv"); src != "npm:@scope/srv" || launch != nil {
		t.Errorf("parseToolArg = %q %v, want the bare source and no launch", src, launch)
	}
}

func TestRefAlias(t *testing.T) {
	cases := map[string]string{
		"@me/github-tools:0.1":          "github-tools",
		"io.github.foo/github-tools":    "github-tools",
		"@me/mcp-server-time-tools:0.1": "mcp-server-time-tools",
	}
	for ref, want := range cases {
		if got := refAlias(ref); got != want {
			t.Errorf("refAlias(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestReuseSearchTerm(t *testing.T) {
	cases := map[string]string{
		"npm:@modelcontextprotocol/server-time": "server-time",
		"io.github.foo/server-time":             "server-time",
		"pypi:mcp-server-time":                  "mcp-server-time",
		"bareword":                              "bareword",
	}
	for origin, want := range cases {
		if got := reuseSearchTerm(origin); got != want {
			t.Errorf("reuseSearchTerm(%q) = %q, want %q", origin, got, want)
		}
	}
}

func TestCandidateProvenance(t *testing.T) {
	if got := candidateProvenance(wrapperCandidate{Local: true}); got != "local" {
		t.Errorf("provenance = %q, want local", got)
	}
	if got := candidateProvenance(wrapperCandidate{Eval: "47/50 j0.83"}); got != "registry, evals 47/50 j0.83" {
		t.Errorf("provenance = %q, want registry with eval", got)
	}
}
