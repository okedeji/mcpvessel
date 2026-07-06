package mcpregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/env"
)

func TestExchangeGitHubToken(t *testing.T) {
	stub := &stubRegistry{regToken: "reg-jwt-xyz", regExpires: 4102444800}
	c := newStub(t, stub)
	tok, err := c.ExchangeGitHubToken(context.Background(), "gh-access-abc")
	if err != nil {
		t.Fatalf("ExchangeGitHubToken: %v", err)
	}
	if stub.gotGHToken != "gh-access-abc" {
		t.Errorf("registry got github token %q, want the one sent", stub.gotGHToken)
	}
	if tok.Value != "reg-jwt-xyz" {
		t.Errorf("token value = %q, want the registry token", tok.Value)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("expiry not set from expires_at")
	}
}

func TestExchangeGitHubToken_NoTokenBack(t *testing.T) {
	c := newStub(t, &stubRegistry{regToken: ""})
	if _, err := c.ExchangeGitHubToken(context.Background(), "gh"); err == nil {
		t.Fatal("want an error when the registry returns no token")
	}
}

func TestSaveLoadToken_RoundTrip(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	if err := SaveToken(Token{Value: "reg-jwt-xyz"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	tok, found, err := LoadToken()
	if err != nil || !found {
		t.Fatalf("LoadToken: found=%v err=%v", found, err)
	}
	if tok.Value != "reg-jwt-xyz" {
		t.Errorf("loaded value = %q, want the saved token", tok.Value)
	}
}

func TestLoadToken_MissingIsNotFound(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	_, found, err := LoadToken()
	if err != nil || found {
		t.Fatalf("LoadToken on empty home: found=%v err=%v, want not found and no error", found, err)
	}
}

func TestToken_Redacts(t *testing.T) {
	tok := Token{Value: "super-secret-bearer"}
	if s := fmt.Sprintf("%v %#v", tok, tok); strings.Contains(s, "super-secret-bearer") {
		t.Errorf("formatted token leaked the value: %q", s)
	}
	raw, _ := json.Marshal(tok)
	if strings.Contains(string(raw), "super-secret-bearer") {
		t.Errorf("marshaled token leaked the value: %q", raw)
	}
}
