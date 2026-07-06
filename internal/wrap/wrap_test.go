package wrap

import (
	"strings"
	"testing"
)

func TestAgentfile_NPM(t *testing.T) {
	got, err := Agentfile(Source{Registry: NPM, Identifier: "@modelcontextprotocol/server-filesystem", Version: "1.0"})
	if err != nil {
		t.Fatalf("Agentfile: %v", err)
	}
	wantContains := []string{
		"FROM node:22-slim",
		"RUN npm install -g @modelcontextprotocol/server-filesystem@1.0",
		"EXPOSE *",
		"ENTRYPOINT npx -y @modelcontextprotocol/server-filesystem",
	}
	for _, w := range wantContains {
		if !strings.Contains(got, w) {
			t.Errorf("Agentfile missing %q; got:\n%s", w, got)
		}
	}
}

func TestAgentfile_PyPIWithEnvAndSecret(t *testing.T) {
	got, err := Agentfile(Source{
		Registry:   PyPI,
		Identifier: "mcp-server-fetch",
		Env: []EnvVar{
			{Name: "USER_AGENT", Default: "agentcage"},
			{Name: "API_KEY", Secret: true},
			{Name: "BASE_URL"},
		},
	})
	if err != nil {
		t.Fatalf("Agentfile: %v", err)
	}
	for _, w := range []string{
		"FROM python:3.12-slim",
		"RUN pip install --no-cache-dir mcp-server-fetch\n",
		"SECRETS API_KEY",
		"ENV USER_AGENT=agentcage",
		"ENV BASE_URL\n",
		"ENTRYPOINT mcp-server-fetch",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("Agentfile missing %q; got:\n%s", w, got)
		}
	}
}

func TestAgentfile_DescribesInputs(t *testing.T) {
	got, err := Agentfile(Source{
		Registry:   PyPI,
		Identifier: "srv",
		Env:        []EnvVar{{Name: "API_KEY", Secret: true, Description: "The service API key."}},
	})
	if err != nil {
		t.Fatalf("Agentfile: %v", err)
	}
	// The description rides as a comment directly above the SECRETS line.
	if !strings.Contains(got, "# The service API key.\nSECRETS API_KEY") {
		t.Errorf("Agentfile does not document the input; got:\n%s", got)
	}
}

func TestAgentfile_OCINeedsLaunch(t *testing.T) {
	if _, err := Agentfile(Source{Registry: OCI, Identifier: "ghcr.io/acme/mcp", Version: "1.2"}); err == nil {
		t.Fatal("want an error: oci wrap has no launch command")
	}
	got, err := Agentfile(Source{Registry: OCI, Identifier: "ghcr.io/acme/mcp", Version: "1.2", Launch: []string{"mcp-slack", "--stdio"}})
	if err != nil {
		t.Fatalf("Agentfile: %v", err)
	}
	if !strings.Contains(got, "FROM ghcr.io/acme/mcp:1.2") || !strings.Contains(got, "ENTRYPOINT mcp-slack --stdio") {
		t.Errorf("oci Agentfile wrong; got:\n%s", got)
	}
}

func TestAgentfile_UnsupportedRegistry(t *testing.T) {
	if _, err := Agentfile(Source{Registry: "cargo", Identifier: "x"}); err == nil {
		t.Fatal("want an error for an unsupported registry type")
	}
}

func TestParseCoordinate(t *testing.T) {
	cases := []struct {
		in         string
		wantOK     bool
		reg, id, v string
	}{
		{"npm:@modelcontextprotocol/server-filesystem@1.0", true, NPM, "@modelcontextprotocol/server-filesystem", "1.0"},
		{"npm:@scope/pkg", true, NPM, "@scope/pkg", ""},
		{"npm:plain@2.3", true, NPM, "plain", "2.3"},
		{"pypi:mcp-server-fetch==0.2", true, PyPI, "mcp-server-fetch", "0.2"},
		{"oci:ghcr.io/acme/mcp:1.2", true, OCI, "ghcr.io/acme/mcp", "1.2"},
		{"oci:ghcr.io/acme/mcp@sha256:abc", true, OCI, "ghcr.io/acme/mcp", "sha256:abc"},
		{"io.github.user/server", false, "", "", ""},
		{"ghcr.io/org/name:1.0", false, "", "", ""},
	}
	for _, c := range cases {
		src, ok, err := ParseCoordinate(c.in)
		if err != nil {
			t.Fatalf("ParseCoordinate(%q): %v", c.in, err)
		}
		if ok != c.wantOK {
			t.Errorf("ParseCoordinate(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (src.Registry != c.reg || src.Identifier != c.id || src.Version != c.v) {
			t.Errorf("ParseCoordinate(%q) = %+v, want reg=%q id=%q v=%q", c.in, src, c.reg, c.id, c.v)
		}
	}
}
