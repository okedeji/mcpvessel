package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/reference"
)

// nonInteractiveCmd is a command whose stdin is a buffer, so isInteractive is
// false and confirmLoginIfNeeded takes its non-prompting branches.
func nonInteractiveCmd(stdin string) *cobra.Command {
	c := &cobra.Command{}
	c.SetIn(bytes.NewBufferString(stdin))
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	return c
}

func setClientID(t *testing.T) {
	t.Helper()
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	c.SetEnv(env.GitHubClientID, "Ov23test")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmLoginIfNeeded_LoggedInProceeds(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	if err := mcpregistry.SaveToken(mcpregistry.Token{Value: "t"}); err != nil {
		t.Fatal(err)
	}
	ok, err := confirmLoginIfNeeded(nonInteractiveCmd(""), false)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want a live token to proceed", ok, err)
	}
}

func TestConfirmLoginIfNeeded_NoAppMustPublishErrors(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	_, err := confirmLoginIfNeeded(nonInteractiveCmd(""), true)
	if err == nil || !strings.Contains(err.Error(), "no MCP Registry app") {
		t.Fatalf("err=%v, want a no-app error when publish is required", err)
	}
}

func TestConfirmLoginIfNeeded_NoAppOptionalSkipsCleanly(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	ok, err := confirmLoginIfNeeded(nonInteractiveCmd(""), false)
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want a clean skip (push continues without publishing)", ok, err)
	}
}

func TestConfirmLoginIfNeeded_AppSetNotLoggedInMustPublishErrors(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	setClientID(t)
	_, err := confirmLoginIfNeeded(nonInteractiveCmd(""), true)
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err=%v, want a not-logged-in error", err)
	}
}

func TestConfirm_ReadsYesNo(t *testing.T) {
	cases := map[string]bool{"y\n": true, "yes\n": true, "\n": true, "n\n": false, "no\n": false, "maybe\n": false}
	for in, want := range cases {
		if got := confirm(nonInteractiveCmd(in), "?"); got != want {
			t.Errorf("confirm(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPublishDecision(t *testing.T) {
	cases := []struct {
		host              string
		public, private   bool
		wantPublish       bool
		wantReasonNonZero bool
	}{
		{"ghcr.io", false, false, true, false},                // public host attempts; the publish gate verifies visibility
		{"ghcr.io", true, false, true, false},                 // --public forces
		{"docker.io", false, false, true, false},              // auto-publish on a public host
		{"quay.io", false, false, true, false},                // auto-publish on a public host
		{"registry.acme.internal", false, false, false, true}, // private host
		{"docker.io", false, true, false, false},              // --private opts out silently
	}
	for _, c := range cases {
		got, reason := publishDecision(c.host, c.public, c.private)
		if got != c.wantPublish {
			t.Errorf("publishDecision(%q, pub=%v, priv=%v) publish = %v, want %v", c.host, c.public, c.private, got, c.wantPublish)
		}
		if (reason != "") != c.wantReasonNonZero {
			t.Errorf("publishDecision(%q, ...) reason = %q, wantNonZero = %v", c.host, reason, c.wantReasonNonZero)
		}
	}
}

// buildBundleFile writes a minimal agent bundle to a fresh .agent file.
func buildBundleFile(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Agentfile"), []byte("FROM x\nMAIN respond\nENTRYPOINT y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "r.agent")
	if err := bundle.Build(src, file); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return file
}

func TestPublishToRegistry_RoundTrip(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())

	var got mcpregistry.Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0.1/publish" {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	t.Setenv(env.MCPRegistry, ts.URL)
	if err := mcpregistry.SaveToken(mcpregistry.Token{Value: "tok"}); err != nil {
		t.Fatal(err)
	}

	ref, err := reference.Parse("ghcr.io/me/researcher:0.1")
	if err != nil {
		t.Fatal(err)
	}
	publicOK := func(context.Context, reference.Reference) (string, error) { return "sha256:abc", nil }
	var out bytes.Buffer
	if err := publishToRegistryWith(context.Background(), &out, ref, buildBundleFile(t), "", publicOK); err != nil {
		t.Fatalf("publishToRegistryWith: %v", err)
	}
	if got.Name != "io.github.me/researcher" {
		t.Errorf("published name = %q, want the derived reverse-DNS name", got.Name)
	}
	if _, _, ok := got.OCIReference(); !ok {
		t.Error("published server has no oci package pointing at the bundle")
	}
	if !strings.Contains(out.String(), "Published") {
		t.Errorf("output = %q, want a publish confirmation", out.String())
	}
}

func TestPublishToRegistry_UnpushedArtifactRefused(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	if err := mcpregistry.SaveToken(mcpregistry.Token{Value: "tok"}); err != nil {
		t.Fatal(err)
	}
	ref, err := reference.Parse("ghcr.io/me/researcher:0.1")
	if err != nil {
		t.Fatal(err)
	}
	notPublic := func(context.Context, reference.Reference) (string, error) {
		return "", errors.New("unauthorized")
	}
	err = publishToRegistryWith(context.Background(), io.Discard, ref, buildBundleFile(t), "", notPublic)
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("err = %v, want a refusal telling the operator to push first", err)
	}
}

func TestPublishToRegistry_NotLoggedIn(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	ref, err := reference.Parse("ghcr.io/me/researcher:0.1")
	if err != nil {
		t.Fatal(err)
	}
	err = publishToRegistry(context.Background(), io.Discard, ref, buildBundleFile(t), "")
	if err == nil || !strings.Contains(err.Error(), "login mcp-registry") {
		t.Fatalf("err = %v, want a login hint", err)
	}
}

func TestPublishToRegistry_UnderivableNameNeedsOverride(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	ref, err := reference.Parse("docker.io/me/researcher:0.1")
	if err != nil {
		t.Fatal(err)
	}
	err = publishToRegistry(context.Background(), io.Discard, ref, buildBundleFile(t), "")
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want a request to pass --name", err)
	}
}
