package mcpregistry

import (
	"sort"
	"strings"
)

// The registry matches a search as a substring of the whole reverse-DNS name
// and returns the hits in name order, with no relevance signal of its own. That
// combination loses the server the caller meant: nearly every entry published
// from GitHub is named "io.github.<user>/<package>", so a bare "github" matches
// most of the catalog and the alphabetical page runs out long before
// "io.github.github/github-mcp-server". This file is the client-side answer:
// probe the registry in a way that reaches the right candidates, then rank them.

// searchPool is how many candidates one probe pulls before ranking. The
// registry caps a page at 100, so this is the widest single look; the caller's
// limit trims the ranked result afterwards.
const searchPool = 100

// searchProbes returns the registry queries whose union is searched for one
// user query. The bare query is always issued. A single bare word is also
// issued anchored at the package-name boundary ("/github"), which narrows the
// match to servers actually named that instead of every server published by a
// GitHub user. Anything already carrying a "/" names the package half itself,
// and a multi-word query is not a package name, so neither gets the anchor.
func searchProbes(query string) []string {
	q := strings.TrimSpace(query)
	if q == "" {
		return []string{""}
	}
	probes := []string{q}
	if !strings.ContainsAny(q, "/ \t") {
		probes = append(probes, "/"+q)
	}
	return probes
}

// packageSegment is the name's package half: everything after the last "/".
// For "io.github.github/github-mcp-server" that is "github-mcp-server".
func packageSegment(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// ownerSegment is the publisher's own label: the last dotted piece of the
// reverse-DNS namespace. For "io.github.github/github-mcp-server" that is
// "github", the org that published it; the "io.github." prefix in front of it
// is carried by every GitHub publisher and says nothing about who.
func ownerSegment(name string) string {
	ns := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		ns = name[:i]
	}
	if i := strings.LastIndex(ns, "."); i >= 0 {
		return ns[i+1:]
	}
	return ns
}

// relevance scores one server against the query. It reads a name as its two
// meaningful halves (package and publisher) and scores how squarely the query
// names either, so an exact package name beats a prefix, a prefix beats a
// substring, and a description-only hit comes last.
func relevance(s Server, query string) int {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0
	}
	name := strings.ToLower(s.Name)
	pkg := packageSegment(name)
	owner := ownerSegment(name)

	score := 0
	switch {
	case pkg == q:
		score = 100
	case strings.HasPrefix(pkg, q+"-"), strings.HasPrefix(pkg, q+"_"):
		score = 80
	case strings.HasPrefix(pkg, q):
		score = 60
	case strings.Contains(pkg, q):
		score = 40
	case owner == q:
		score = 30
	case strings.Contains(strings.ToLower(s.Description), q):
		score = 10
	}
	// Nothing about this server names the query, so no bonus can rescue it. The
	// registry's substring match hands back entries whose only tie to "github"
	// is the io.github. prefix every GitHub publisher carries, and letting a
	// tiebreak lift one of those above zero is what buried the real hit.
	if score == 0 {
		return 0
	}
	// A publisher whose own namespace is the query is that project's first
	// party: "github" publishing io.github.github/github-mcp-server. It is the
	// only authority signal a name carries, and it is what separates the
	// official server from an unrelated publisher's package of the same name.
	if owner == q {
		score += 25
	}
	// Search exists to find something to cage, and a remote-only entry cannot
	// be. Small, so it orders ties rather than overriding a better name match.
	if s.Cageable() {
		score += 5
	}
	return score
}

// rankSearch orders candidates by relevance to query and trims to limit (all of
// them when limit is not positive). Ties break on name so repeated searches
// return the same order. A candidate that matches nothing at all is dropped:
// the registry's substring match admits entries whose only tie to the query is
// the "io.github." prefix, and listing those is what buried the real hit.
func rankSearch(servers []Server, query string, limit int) []Server {
	type scored struct {
		server Server
		score  int
	}
	ranked := make([]scored, 0, len(servers))
	for _, s := range servers {
		score := relevance(s, query)
		if score == 0 && strings.TrimSpace(query) != "" {
			continue
		}
		ranked = append(ranked, scored{server: s, score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].server.Name < ranked[j].server.Name
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Server, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.server)
	}
	return out
}
