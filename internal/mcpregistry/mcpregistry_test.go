package mcpregistry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/env"
)

// stubRegistry stands in for the MCP Registry: search returns whatever servers
// the test seeds, publish records the last request it saw.
//
// mu guards the recorded fields. One discovery search issues its probes
// concurrently, so the handler runs on more than one goroutine at a time.
type stubRegistry struct {
	mu         sync.Mutex
	servers    []Server
	gotAuth    string
	gotServer  Server
	publishSt  int
	gotGHToken string
	regToken   string
	regExpires int64

	// latestVersion, when set, is the version the stub stamps isLatest on.
	latestVersion string
	// ignoreLatest makes the stub serve every version even when asked for
	// version=latest, standing in for a registry that does not know the filter.
	ignoreLatest bool
	gotVersionQ  string
}

func newStub(t *testing.T, s *stubRegistry) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0.1/servers":
			match := r.URL.Query().Get("search")
			s.mu.Lock()
			s.gotVersionQ = r.URL.Query().Get("version")
			// Only a stub told which version is current can collapse to it; a
			// test that seeds one entry per name serves them all either way.
			onlyLatest := s.gotVersionQ == "latest" && !s.ignoreLatest && s.latestVersion != ""
			var out serverList
			for _, srv := range s.servers {
				if match != "" && !strings.Contains(srv.Name, match) {
					continue
				}
				latest := s.latestVersion != "" && srv.Version == s.latestVersion
				if onlyLatest && !latest {
					continue
				}
				e := serverEnvelope{Server: srv}
				if latest {
					e.Meta = map[string]any{officialMetaKey: map[string]any{officialIsLatestKey: true}}
				}
				out.Servers = append(out.Servers, e)
			}
			out.Metadata.Count = len(out.Servers)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/v0.1/publish":
			s.mu.Lock()
			s.gotAuth = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &s.gotServer)
			if s.publishSt == 0 {
				s.publishSt = http.StatusOK
			}
			status := s.publishSt
			s.mu.Unlock()
			w.WriteHeader(status)
		case r.Method == http.MethodPost && r.URL.Path == "/v0.1/auth/github-at":
			var body struct {
				GitHubToken string `json:"github_token"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			s.gotGHToken = body.GitHubToken
			_ = json.NewEncoder(w).Encode(map[string]any{"registry_token": s.regToken, "expires_at": s.regExpires})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	t.Setenv(env.MCPRegistry, ts.URL)
	return New()
}

func TestSearch_ReturnsMatches(t *testing.T) {
	c := newStub(t, &stubRegistry{servers: []Server{
		{Name: "io.github.a/filesystem", Description: "files"},
		{Name: "io.github.b/weather", Description: "weather"},
	}})
	got, err := c.Search(context.Background(), "filesystem", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Name != "io.github.a/filesystem" {
		t.Fatalf("Search = %+v, want the one filesystem match", got)
	}
}

func TestResolve_ExactMatchAndOCIReference(t *testing.T) {
	c := newStub(t, &stubRegistry{servers: []Server{
		{Name: "io.github.a/fs", Packages: []Package{{RegistryType: "oci", Identifier: "ghcr.io/a/fs", Version: "0.1"}}},
		{Name: "io.github.a/fs-extra"},
	}})
	got, err := c.Resolve(context.Background(), "io.github.a/fs")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	ref, version, ok := got.OCIReference()
	if !ok || ref != "ghcr.io/a/fs" || version != "0.1" {
		t.Fatalf("OCIReference = %q %q %v, want the oci package", ref, version, ok)
	}
}

// The registry holds one entry per version and returns them oldest first, so
// resolving a bare name must not take the first match: that pinned every caller
// to a publisher's earliest release. Regression for docs 0.1.0 being served
// after 0.1.2 shipped.
func TestResolve_PicksLatestNotFirst(t *testing.T) {
	stub := &stubRegistry{
		latestVersion: "0.1.2",
		servers: []Server{
			{Name: "io.github.a/fs", Version: "0.1.0"},
			{Name: "io.github.a/fs", Version: "0.1.1"},
			{Name: "io.github.a/fs", Version: "0.1.2"},
		},
	}
	c := newStub(t, stub)
	got, err := c.Resolve(context.Background(), "io.github.a/fs")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Version != "0.1.2" {
		t.Errorf("resolved version = %q, want the latest 0.1.2", got.Version)
	}
	if stub.gotVersionQ != "latest" {
		t.Errorf("version query = %q, want the registry asked for the latest", stub.gotVersionQ)
	}
}

// A registry that does not know the version filter serves every version anyway;
// the ordering fallback still has to land on the newest, not the oldest.
func TestResolve_FallsBackToLastWhenFilterIgnored(t *testing.T) {
	c := newStub(t, &stubRegistry{
		ignoreLatest: true,
		servers: []Server{
			{Name: "io.github.a/fs", Version: "0.1.0"},
			{Name: "io.github.a/fs", Version: "0.1.2"},
		},
	})
	got, err := c.Resolve(context.Background(), "io.github.a/fs")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Version != "0.1.2" {
		t.Errorf("resolved version = %q, want the newest by publish order", got.Version)
	}
}

// The isLatest stamp wins over position, so a registry that returns versions in
// any other order still resolves correctly.
func TestResolve_PrefersIsLatestOverPosition(t *testing.T) {
	c := newStub(t, &stubRegistry{
		ignoreLatest:  true,
		latestVersion: "0.1.1",
		servers: []Server{
			{Name: "io.github.a/fs", Version: "0.1.1"},
			{Name: "io.github.a/fs", Version: "0.1.0"},
		},
	})
	got, err := c.Resolve(context.Background(), "io.github.a/fs")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Version != "0.1.1" {
		t.Errorf("resolved version = %q, want the isLatest-stamped entry", got.Version)
	}
}

// search lists every version; only Resolve collapses a name to one.
func TestSearch_ListsEveryVersion(t *testing.T) {
	stub := &stubRegistry{latestVersion: "0.1.2", servers: []Server{
		{Name: "io.github.a/fs", Version: "0.1.0"},
		{Name: "io.github.a/fs", Version: "0.1.2"},
	}}
	c := newStub(t, stub)
	got, err := c.Search(context.Background(), "fs", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Search returned %d entries, want both versions listed", len(got))
	}
	if stub.gotVersionQ != "" {
		t.Errorf("version query = %q, want search to not collapse versions", stub.gotVersionQ)
	}
}

func TestResolve_NotFound(t *testing.T) {
	c := newStub(t, &stubRegistry{servers: []Server{{Name: "io.github.a/fs"}}})
	_, err := c.Resolve(context.Background(), "io.github.a/absent")
	if err == nil || !strings.Contains(err.Error(), "no such server") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

func TestPublish_SendsBearerAndBody(t *testing.T) {
	stub := &stubRegistry{}
	c := newStub(t, stub)
	s := &Server{Name: "io.github.a/fs", Description: "files", Version: "0.1"}
	if err := c.Publish(context.Background(), s, "tok-123"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if stub.gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want the bearer token", stub.gotAuth)
	}
	if stub.gotServer.Name != "io.github.a/fs" {
		t.Errorf("published name = %q, want the server name", stub.gotServer.Name)
	}
}

func TestPublish_NoTokenFailsClosed(t *testing.T) {
	c := newStub(t, &stubRegistry{})
	err := c.Publish(context.Background(), &Server{Name: "io.github.a/fs"}, "")
	if err == nil || !strings.Contains(err.Error(), "login mcp-registry") {
		t.Fatalf("err = %v, want a login hint", err)
	}
}

func TestPublish_TokenRejected(t *testing.T) {
	c := newStub(t, &stubRegistry{publishSt: http.StatusUnauthorized})
	err := c.Publish(context.Background(), &Server{Name: "io.github.a/fs"}, "stale")
	if err == nil || !strings.Contains(err.Error(), "again") {
		t.Fatalf("err = %v, want a re-login hint", err)
	}
}

func TestServerJSONFromManifest_MapsFields(t *testing.T) {
	passed := 47
	m := bundle.Manifest{
		Vesselfile: bundle.VesselfileSpec{Meta: map[string]string{"description": "a filesystem agent"}},
		Evals:      &bundle.Evals{Declared: true, Passed: &passed},
	}
	s := ServerJSONFromManifest(m, "io.github.a/fs", "ghcr.io/a/fs", "0.1")
	if s.Name != "io.github.a/fs" || s.Version != "0.1" || s.Description != "a filesystem agent" {
		t.Fatalf("server = %+v, want mapped name/version/description", s)
	}
	if s.Schema == "" {
		t.Error("$schema is required by the registry on publish but was not set")
	}
	// The version rides in the identifier, per the registry's rule.
	ref, version, ok := s.OCIReference()
	if !ok || ref != "ghcr.io/a/fs:0.1" || version != "" {
		t.Errorf("OCIReference = %q %q %v, want the version embedded in the identifier", ref, version, ok)
	}
	// Evals ride inside the publisher-provided slot, not as a top-level _meta
	// key (the registry rejects sibling keys outside its own namespaces).
	provided, ok := s.Meta[publisherMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing the publisher-provided slot")
	}
	if _, ok := provided[providedEvalsKey]; !ok {
		t.Errorf("publisher-provided missing the evals key %q", providedEvalsKey)
	}
}

func TestServerJSONFromManifest_StampsImportedFrom(t *testing.T) {
	m := bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Meta: map[string]string{"imported_from": "npm:@scope/server-time"}}}
	s := ServerJSONFromManifest(m, "io.github.a/time", "ghcr.io/a/time", "0.1")
	if got := s.ImportedFrom(); got != "npm:@scope/server-time" {
		t.Errorf("ImportedFrom = %q, want the marker stamped into _meta and read back", got)
	}

	// A non-wrapper carries no marker.
	plain := ServerJSONFromManifest(bundle.Manifest{}, "io.github.a/x", "ghcr.io/a/x", "0.1")
	if got := plain.ImportedFrom(); got != "" {
		t.Errorf("ImportedFrom = %q, want empty for a non-wrapper", got)
	}
}

func TestServerJSONFromManifest_DescriptionFallbackAndClamp(t *testing.T) {
	long := bundle.Manifest{Vesselfile: bundle.VesselfileSpec{Meta: map[string]string{"description": strings.Repeat("x", 200)}}}
	if got := ServerJSONFromManifest(long, "io.github.a/fs", "ghcr.io/a/fs", "0.1").Description; len(got) != maxDescription {
		t.Errorf("description len = %d, want clamped to %d", len(got), maxDescription)
	}
	none := bundle.Manifest{}
	if got := ServerJSONFromManifest(none, "io.github.a/fs", "ghcr.io/a/fs", "0.1").Description; got == "" {
		t.Error("description empty, want a name-derived fallback")
	}
}

// SOURCE is what tells a caller whether an entry can be caged at all. Roughly
// half the public registry is remote-only, so getting this wrong sends people
// straight into an import that cannot succeed.
func TestServerSource_NamesWhereTheCodeLives(t *testing.T) {
	for _, tc := range []struct {
		name     string
		server   Server
		want     string
		cageable bool
	}{
		{
			name:     "a package entry names its ecosystem",
			server:   Server{Packages: []Package{{RegistryType: "pypi", Identifier: "mcp-server-fetch"}}},
			want:     "pypi",
			cageable: true,
		},
		{
			name:     "several ecosystems are joined, not truncated",
			server:   Server{Packages: []Package{{RegistryType: "npm"}, {RegistryType: "pypi"}}},
			want:     "npm+pypi",
			cageable: true,
		},
		{
			name:     "a duplicate ecosystem is listed once",
			server:   Server{Packages: []Package{{RegistryType: "npm"}, {RegistryType: "npm"}}},
			want:     "npm",
			cageable: true,
		},
		{
			name:     "a hosted URL has no code to cage",
			server:   Server{Remotes: []Remote{{Type: "streamable-http", URL: "https://example.com/mcp"}}},
			want:     remoteSource,
			cageable: false,
		},
		{
			// A package entry that also offers a hosted URL is still cageable;
			// the local code is what matters.
			name:     "packages win over remotes",
			server:   Server{Packages: []Package{{RegistryType: "npm"}}, Remotes: []Remote{{Type: "streamable-http"}}},
			want:     "npm",
			cageable: true,
		},
		{name: "neither declared", server: Server{}, want: "", cageable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.server.Source(); got != tc.want {
				t.Errorf("Source() = %q, want %q", got, tc.want)
			}
			if got := tc.server.Cageable(); got != tc.cageable {
				t.Errorf("Cageable() = %v, want %v", got, tc.cageable)
			}
		})
	}
}

// Discovery collapses a name to its current version. The registry stores one
// entry per version, and without this a search for a popular server spends its
// whole limit listing that one server's history.
func TestSearchLatest_CollapsesVersions(t *testing.T) {
	stub := &stubRegistry{latestVersion: "1.1.2", servers: []Server{
		{Name: "cloud.fetcher/fetcher", Version: "1.0.0"},
		{Name: "cloud.fetcher/fetcher", Version: "1.1.0"},
		{Name: "cloud.fetcher/fetcher", Version: "1.1.2"},
	}}
	c := newStub(t, stub)
	got, complete, err := c.SearchLatest(context.Background(), "fetcher", 0)
	if err != nil {
		t.Fatalf("SearchLatest: %v", err)
	}
	if !complete {
		t.Fatal("SearchLatest reported incomplete results though every probe answered")
	}
	if len(got) != 1 || got[0].Version != "1.1.2" {
		t.Fatalf("SearchLatest = %+v, want the one current version", got)
	}
}
