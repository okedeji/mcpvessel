package mcpregistry

import (
	"strings"
	"testing"
)

func TestSearchProbes_AnchorsABareWord(t *testing.T) {
	got := searchProbes("github")
	want := []string{"github", "/github"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("searchProbes(github) = %v, want %v", got, want)
	}
}

func TestSearchProbes_LeavesAPreciseQueryAlone(t *testing.T) {
	// A query that already names the package half, or spans words, gains
	// nothing from the anchor and would only spend a second registry call.
	for _, q := range []string{"io.github.github/github-mcp-server", "web search"} {
		if got := searchProbes(q); len(got) != 1 || got[0] != q {
			t.Fatalf("searchProbes(%q) = %v, want just the query", q, got)
		}
	}
}

func TestOwnerAndPackageSegments(t *testing.T) {
	const name = "io.github.github/github-mcp-server"
	if got := packageSegment(name); got != "github-mcp-server" {
		t.Fatalf("packageSegment = %q", got)
	}
	// Not "io.github": the prefix every GitHub publisher carries says nothing
	// about who published, so the owner is the label after it.
	if got := ownerSegment(name); got != "github" {
		t.Fatalf("ownerSegment = %q", got)
	}
}

// TestRankSearch_SurfacesTheFirstPartyServer is the case that made this file
// exist: a search for "github" used to bury the official server under every
// io.github.<user>/* entry in the catalog, because the registry substring-
// matches the whole name and orders alphabetically.
func TestRankSearch_SurfacesTheFirstPartyServer(t *testing.T) {
	official := Server{
		Name:     "io.github.github/github-mcp-server",
		Packages: []Package{{RegistryType: "oci", Identifier: "ghcr.io/github/github-mcp-server"}},
	}
	servers := []Server{
		{Name: "io.github.0Mattias/bettermemory", Packages: []Package{{RegistryType: "npm", Identifier: "bettermemory"}}},
		{Name: "io.github.06ketan/slideshot", Packages: []Package{{RegistryType: "npm", Identifier: "slideshot"}}},
		{Name: "ai.smithery/smithery-ai-github", Remotes: []Remote{{Type: "streamable-http", URL: "https://example.test/mcp"}}},
		official,
		{Name: "io.github.crypto-ninja/github-mcp-server", Packages: []Package{{RegistryType: "npm", Identifier: "gh"}}},
	}

	got := rankSearch(servers, "github", 20)
	if len(got) == 0 {
		t.Fatal("rankSearch dropped everything")
	}
	if got[0].Name != official.Name {
		t.Fatalf("rankSearch put %q first, want the first-party %q", got[0].Name, official.Name)
	}
	// The two entries whose only tie to "github" is the io.github. prefix are
	// noise, and listing them is what buried the real hit.
	for _, s := range got {
		if strings.Contains(s.Name, "bettermemory") || strings.Contains(s.Name, "slideshot") {
			t.Fatalf("rankSearch kept prefix-only noise: %q", s.Name)
		}
	}
}

func TestRankSearch_ExactPackageBeatsPrefix(t *testing.T) {
	exact := Server{Name: "com.mcparmory/github", Packages: []Package{{RegistryType: "pypi", Identifier: "github"}}}
	prefixed := Server{Name: "io.github.someone/github-tools", Packages: []Package{{RegistryType: "npm", Identifier: "gh"}}}
	got := rankSearch([]Server{prefixed, exact}, "github", 0)
	if len(got) != 2 || got[0].Name != exact.Name {
		t.Fatalf("rankSearch = %v, want the exact package name first", names(got))
	}
}

func TestRankSearch_PrefersACageableEntryOverARemoteOne(t *testing.T) {
	remote := Server{Name: "a.remote/thing", Remotes: []Remote{{Type: "streamable-http", URL: "https://example.test/mcp"}}}
	cageable := Server{Name: "z.local/thing", Packages: []Package{{RegistryType: "npm", Identifier: "thing"}}}
	got := rankSearch([]Server{remote, cageable}, "thing", 0)
	// Both are exact package matches, so only cageability separates them, and
	// it must beat the alphabetical tiebreak that would otherwise apply.
	if len(got) != 2 || got[0].Name != cageable.Name {
		t.Fatalf("rankSearch = %v, want the cageable entry first", names(got))
	}
}

func TestRankSearch_TrimsToLimitAndIsStable(t *testing.T) {
	servers := []Server{
		{Name: "b.pub/thing", Packages: []Package{{RegistryType: "npm", Identifier: "thing"}}},
		{Name: "a.pub/thing", Packages: []Package{{RegistryType: "npm", Identifier: "thing"}}},
		{Name: "c.pub/thing", Packages: []Package{{RegistryType: "npm", Identifier: "thing"}}},
	}
	got := rankSearch(servers, "thing", 2)
	if len(got) != 2 {
		t.Fatalf("rankSearch returned %d results, want 2", len(got))
	}
	if got[0].Name != "a.pub/thing" || got[1].Name != "b.pub/thing" {
		t.Fatalf("rankSearch = %v, want equal scores broken by name", names(got))
	}
}

func TestRankSearch_EmptyQueryKeepsEverything(t *testing.T) {
	// An empty query lists the catalog rather than filtering it, so the
	// score-zero drop must not apply.
	servers := []Server{{Name: "a.pub/one"}, {Name: "b.pub/two"}}
	if got := rankSearch(servers, "", 0); len(got) != 2 {
		t.Fatalf("rankSearch(empty) = %v, want the catalog intact", names(got))
	}
}

func names(servers []Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}
