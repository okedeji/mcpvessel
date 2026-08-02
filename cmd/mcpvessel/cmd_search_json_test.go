package main

import (
	"encoding/json"
	"testing"

	"github.com/okedeji/mcpvessel/internal/mcpregistry"
)

// The skill tells an agent to read cageable and source from `search --json`.
// They were only ever computed for the human table, so the agent was told to
// read a field that did not exist, and the record already carries a
// repository.source meaning "github" for it to land on instead.
func TestSearchResultJSON_CarriesCageableAndSource(t *testing.T) {
	s := mcpregistry.Server{
		Name:       "com.example/thing",
		Version:    "1.0.0",
		Repository: &mcpregistry.Repository{URL: "https://github.com/example/thing", Source: "github"},
		Packages:   []mcpregistry.Package{{RegistryType: "npm", Identifier: "@example/thing"}},
	}
	row := searchResult{Server: s, Source: s.Source(), Cageable: s.Cageable()}

	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got["source"] != "npm" {
		t.Errorf("source = %v, want the package ecosystem", got["source"])
	}
	if got["cageable"] != true {
		t.Errorf("cageable = %v, want true for a package-backed entry", got["cageable"])
	}
	// The embedded record must keep its shape, including the unrelated
	// repository.source that this field exists to disambiguate.
	if got["name"] != "com.example/thing" {
		t.Errorf("the embedded server record did not flatten: %v", got)
	}
	repo, _ := got["repository"].(map[string]any)
	if repo["source"] != "github" {
		t.Errorf("repository.source = %v, want it untouched", repo["source"])
	}
}

func TestSearchResultJSON_RemoteIsNotCageable(t *testing.T) {
	s := mcpregistry.Server{
		Name:    "ai.smithery/smithery-ai-fetch",
		Remotes: []mcpregistry.Remote{{Type: "streamable-http", URL: "https://server.smithery.ai/fetch/mcp"}},
	}
	row := searchResult{Server: s, Source: s.Source(), Cageable: s.Cageable()}
	if row.Cageable || row.Source != "remote" {
		t.Errorf("row = %+v, want a remote entry marked uncageable", row)
	}
}
