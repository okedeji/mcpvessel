// Package mcpregistry talks to the official MCP Registry: search, resolve
// a reverse-DNS name, publish a server.json. The registry stores no
// bundles; each entry's packages[] point at the OCI artifact, so this
// client moves metadata only, never an agent's bytes.
package mcpregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
)

const (
	defaultBaseURL = "https://registry.modelcontextprotocol.io"

	// apiPrefix moves with the registry's API version, not agentcage's.
	apiPrefix = "/v0.1"

	// requestTimeout bounds a single registry call so a wedged registry
	// fails the command rather than hanging the CLI.
	requestTimeout = 30 * time.Second
)

// Client is a discovery client for the MCP Registry. It holds no
// credentials; Publish takes the caller's bearer per call.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client against the official registry, or the
// AGENTCAGE_MCP_REGISTRY override.
func New() *Client {
	return &Client{
		baseURL: baseURL(),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

func baseURL() string {
	return strings.TrimRight(config.LookupEnvOr(env.MCPRegistry, defaultBaseURL), "/")
}

// Search returns servers matching query, newest first, up to limit. An
// empty query lists the catalog; only the first page is returned.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Server, error) {
	q := url.Values{}
	if query != "" {
		q.Set("search", query)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var list serverList
	if err := c.get(ctx, "/servers", q, &list); err != nil {
		return nil, fmt.Errorf("searching the MCP Registry for %q: %w", query, err)
	}
	out := make([]Server, 0, len(list.Servers))
	for _, e := range list.Servers {
		out = append(out, e.Server)
	}
	return out, nil
}

// Resolve returns the registry entry for an exact reverse-DNS name, or a
// not-found error naming what was asked for.
func (c *Client) Resolve(ctx context.Context, name string) (*Server, error) {
	servers, err := c.Search(ctx, name, 0)
	if err != nil {
		return nil, err
	}
	for i := range servers {
		if servers[i].Name == name {
			return &servers[i], nil
		}
	}
	return nil, fmt.Errorf("resolving %s: no such server in the MCP Registry", name)
}

// Publish records a server.json under the caller's namespace. token is the
// bearer from the GitHub OAuth namespace proof. A rejected token is an
// error, never a silent no-op.
func (c *Client) Publish(ctx context.Context, s *Server, token string) error {
	if token == "" {
		return fmt.Errorf("publishing %s: no MCP Registry token; run 'agentcage login mcp-registry' first", s.Name)
	}
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encoding server.json for %s: %w", s.Name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+apiPrefix+"/publish", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building publish request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("publishing %s: %w", s.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("publishing %s: token rejected; run 'agentcage login mcp-registry' again", s.Name)
	case http.StatusForbidden:
		return fmt.Errorf("publishing %s: token cannot publish this namespace", s.Name)
	default:
		return fmt.Errorf("publishing %s: registry returned %s: %s", s.Name, resp.Status, snippet(resp.Body))
	}
}

// get issues a GET against an API path and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	u := c.baseURL + apiPrefix + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry returned %s: %s", resp.Status, snippet(resp.Body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding registry response: %w", err)
	}
	return nil
}

// snippet reads a bounded prefix of an error body so a misbehaving registry
// cannot flood the terminal.
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
