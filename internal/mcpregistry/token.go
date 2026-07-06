package mcpregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/okedeji/agentcage/internal/env"
)

// authExchangePath is where the official registry trades a GitHub access token
// for its own bearer, per the live registry's OpenAPI: it takes {github_token}
// and returns {registry_token, expires_at}. A mirror that authenticates
// differently is reached by overriding the base URL.
const authExchangePath = "/v0.1/auth/github-at"

// Token is the registry bearer 'login mcp-registry' obtains and publish reads.
// Its value is a credential, so the three methods redact it: a Token logged by
// accident prints as a placeholder, never the bearer itself.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

func (t Token) String() string               { return "mcpregistry.Token([redacted])" }
func (t Token) GoString() string             { return t.String() }
func (t Token) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Expired reports whether the token is known to be past its expiry. A zero
// ExpiresAt means the registry did not say, so it is treated as usable and a
// stale token surfaces as a publish 401 rather than a guess here.
func (t Token) Expired() bool {
	return !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt)
}

// ExchangeGitHubToken trades a GitHub access token for a registry bearer. The
// request and response field names follow the official registry's GitHub
// exchange.
func (c *Client) ExchangeGitHubToken(ctx context.Context, githubToken string) (Token, error) {
	body, err := json.Marshal(map[string]string{"github_token": githubToken})
	if err != nil {
		return Token{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+authExchangePath, bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("exchanging GitHub token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("exchanging GitHub token: registry returned %s: %s", resp.Status, snippet(resp.Body))
	}

	var out struct {
		RegistryToken string `json:"registry_token"`
		ExpiresAt     int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Token{}, fmt.Errorf("decoding token exchange response: %w", err)
	}
	if out.RegistryToken == "" {
		return Token{}, fmt.Errorf("exchanging GitHub token: registry returned no token")
	}
	tok := Token{Value: out.RegistryToken}
	if out.ExpiresAt > 0 {
		tok.ExpiresAt = time.Unix(out.ExpiresAt, 0)
	}
	return tok, nil
}

// tokenFile is the on-disk shape. It is separate from Token so Save persists
// the real value: Token.MarshalJSON redacts, which is exactly what a persisted
// credential must not do.
type tokenFile struct {
	RegistryToken string    `json:"registry_token"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
}

// SaveToken writes the bearer to a 0600 file under ~/.agentcage, the same
// permission the secret store uses, so a login persists across commands without
// widening who can read it.
func SaveToken(t Token) error {
	path, err := tokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(tokenFile{RegistryToken: t.Value, ExpiresAt: t.ExpiresAt}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("writing registry token: %w", err)
	}
	return nil
}

// LoadToken reads the stored bearer. A missing file is "not logged in", not an
// error, so a caller can tell the operator to run login rather than surfacing a
// stat failure.
func LoadToken() (tok Token, found bool, err error) {
	path, err := tokenPath()
	if err != nil {
		return Token{}, false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("reading registry token: %w", err)
	}
	var f tokenFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return Token{}, false, fmt.Errorf("parsing registry token: %w", err)
	}
	return Token{Value: f.RegistryToken, ExpiresAt: f.ExpiresAt}, true, nil
}

func tokenPath() (string, error) {
	home, err := env.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "mcpregistry-token.json"), nil
}
