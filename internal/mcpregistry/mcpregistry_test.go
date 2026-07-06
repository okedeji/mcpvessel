package mcpregistry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/env"
)

// stubRegistry stands in for the MCP Registry: search returns whatever servers
// the test seeds, publish records the last request it saw.
type stubRegistry struct {
	servers    []Server
	gotAuth    string
	gotServer  Server
	publishSt  int
	gotGHToken string
	regToken   string
	regExpires int64
}

func newStub(t *testing.T, s *stubRegistry) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0.1/servers":
			match := r.URL.Query().Get("search")
			var out serverList
			for _, srv := range s.servers {
				if match == "" || strings.Contains(srv.Name, match) {
					out.Servers = append(out.Servers, serverEnvelope{Server: srv})
				}
			}
			out.Metadata.Count = len(out.Servers)
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/v0.1/publish":
			s.gotAuth = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &s.gotServer)
			if s.publishSt == 0 {
				s.publishSt = http.StatusOK
			}
			w.WriteHeader(s.publishSt)
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
		Agentfile: bundle.AgentfileSpec{Meta: map[string]string{"description": "a filesystem agent"}},
		Evals:     &bundle.Evals{Declared: true, Passed: &passed},
	}
	s := ServerJSONFromManifest(m, "io.github.a/fs", "ghcr.io/a/fs", "0.1")
	if s.Name != "io.github.a/fs" || s.Version != "0.1" || s.Description != "a filesystem agent" {
		t.Fatalf("server = %+v, want mapped name/version/description", s)
	}
	if s.Schema == "" {
		t.Error("$schema is required by the registry on publish but was not set")
	}
	// The OCI package embeds the version in the identifier, per the registry's
	// rule, so no separate version comes back.
	ref, version, ok := s.OCIReference()
	if !ok || ref != "ghcr.io/a/fs:0.1" || version != "" {
		t.Errorf("OCIReference = %q %q %v, want the version embedded in the identifier", ref, version, ok)
	}
	if _, ok := s.Meta[evalsMetaKey]; !ok {
		t.Errorf("meta missing the evals key %q", evalsMetaKey)
	}
}

func TestServerJSONFromManifest_DescriptionFallbackAndClamp(t *testing.T) {
	long := bundle.Manifest{Agentfile: bundle.AgentfileSpec{Meta: map[string]string{"description": strings.Repeat("x", 200)}}}
	if got := ServerJSONFromManifest(long, "io.github.a/fs", "ghcr.io/a/fs", "0.1").Description; len(got) != maxDescription {
		t.Errorf("description len = %d, want clamped to %d", len(got), maxDescription)
	}
	none := bundle.Manifest{}
	if got := ServerJSONFromManifest(none, "io.github.a/fs", "ghcr.io/a/fs", "0.1").Description; got == "" {
		t.Error("description empty, want a name-derived fallback")
	}
}
