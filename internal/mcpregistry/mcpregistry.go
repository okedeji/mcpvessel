// Package mcpregistry talks to the official MCP Registry: search, resolve
// a reverse-DNS name, publish a server.json. The registry stores no
// bundles; each entry's packages[] point at the OCI artifact, so this
// client moves metadata only, never an agent's bytes.
package mcpregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/env"
)

const (
	defaultBaseURL = "https://registry.modelcontextprotocol.io"

	// apiPrefix moves with the registry's API version, not mcpvessel's.
	apiPrefix = "/v0.1"

	// requestTimeout bounds a single registry call so a wedged registry
	// fails the command rather than hanging the CLI.
	requestTimeout = 30 * time.Second

	// readAttempts is how many times a read is tried before giving up. The
	// registry's stalls are not independent per request: they cluster into
	// periods of roughly thirty-five seconds during which every attempt hangs.
	// Measured against the live registry, three attempts at readTimeout covered
	// only twenty-four seconds of that and still lost about one run in six.
	// Five covers past forty, which rides out a whole stall period. Reads only:
	// publish and status changes are not idempotent and are never retried.
	readAttempts = 5
)

// Retry timings, variables so a test can shrink them rather than spend real
// seconds proving the loop works.
var (
	// readTimeout bounds one attempt at a read. It is deliberately far below
	// requestTimeout: the registry intermittently stalls for tens of seconds and
	// then answers 200, so a read is better abandoned early and retried than
	// waited out. Measured on a healthy connection, a normal response lands in
	// about a second, and roughly one in five stalled past thirty.
	readTimeout = 8 * time.Second

	// readBackoff is the pause after a failed attempt.
	readBackoff = 500 * time.Millisecond
)

// Client is a discovery client for the MCP Registry. It holds no
// credentials; Publish takes the caller's bearer per call.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client against the official registry, or the
// VESSEL_MCP_REGISTRY override.
func New() *Client {
	return &Client{
		baseURL: baseURL(),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

func baseURL() string {
	return strings.TrimRight(config.LookupEnvOr(env.MCPRegistry, defaultBaseURL), "/")
}

// Search returns servers matching query, one entry per published version, up
// to limit. An empty query lists the catalog; only the first page is returned.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Server, error) {
	return c.searchServers(ctx, query, limit, false)
}

// SearchLatest is the discovery search: each name collapsed to its current
// version, and the results ordered by how well they match the query.
//
// Two things the raw registry does make it unusable head-on. It stores one
// entry per version, so an unfiltered search spends its limit listing the same
// server five times. And it matches the query as a substring of the whole
// reverse-DNS name and returns hits in name order, so "github" matches every
// server any GitHub user published and the page never reaches the one meant.
// gatherCandidates probes around the second problem and rankSearch orders what
// comes back; see rank.go.
//
// complete is false when one of those probes failed. The registry stalls often
// enough that this is not hypothetical, and a search that lost its anchored
// probe silently degrades to exactly the unusable ordering above, so the caller
// is told rather than left to present a thinner list as the whole answer.
func (c *Client) SearchLatest(ctx context.Context, query string, limit int) (servers []Server, complete bool, err error) {
	candidates, complete, err := c.gatherCandidates(ctx, query)
	if err != nil {
		return nil, false, err
	}
	return rankSearch(candidates, query, limit), complete, nil
}

// gatherCandidates runs the query's probes and merges their hits, deduped by
// name. The probes run concurrently because the registry intermittently stalls
// for tens of seconds, and issuing them in series would expose a search to that
// twice. A probe that fails is skipped and reported through complete; only an
// all-probes failure is an error, so an anchored miss still falls back to the
// broad page rather than failing the command.
func (c *Client) gatherCandidates(ctx context.Context, query string) (servers []Server, complete bool, err error) {
	probes := searchProbes(query)
	results := make([][]serverEnvelope, len(probes))
	errs := make([]error, len(probes))
	var wg sync.WaitGroup
	for i, probe := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.search(ctx, probe, searchPool, true)
		}()
	}
	wg.Wait()

	seen := make(map[string]bool)
	var out []Server
	for _, entries := range results {
		for _, e := range entries {
			if seen[e.Server.Name] {
				continue
			}
			seen[e.Server.Name] = true
			out = append(out, e.Server)
		}
	}
	if len(out) == 0 {
		return nil, false, errors.Join(errs...)
	}
	return out, errors.Join(errs...) == nil, nil
}

func (c *Client) searchServers(ctx context.Context, query string, limit int, latestOnly bool) ([]Server, error) {
	entries, err := c.search(ctx, query, limit, latestOnly)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Server)
	}
	return out, nil
}

// search is Search over the raw envelopes, so a caller that needs the
// registry-assigned metadata (isLatest) can read it. latestOnly asks the
// registry to collapse a name to its current version.
func (c *Client) search(ctx context.Context, query string, limit int, latestOnly bool) ([]serverEnvelope, error) {
	q := url.Values{}
	if query != "" {
		q.Set("search", query)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if latestOnly {
		q.Set("version", "latest")
	}
	var list serverList
	if err := c.get(ctx, "/servers", q, &list); err != nil {
		return nil, fmt.Errorf("searching the MCP Registry for %q: %w", query, err)
	}
	return list.Servers, nil
}

// Resolve returns the current registry entry for an exact reverse-DNS name, or
// a not-found error naming what was asked for.
//
// Picking the right version is the whole job here. The registry holds one entry
// per published version and returns them oldest first, so taking the first
// match would pin every caller to a publisher's earliest release forever. The
// query asks the registry for the latest; the isLatest scan and the trailing
// fallback cover a registry that ignores the filter (a self-hosted one, or an
// older API), where last-returned is the newest by publish order.
func (c *Client) Resolve(ctx context.Context, name string) (*Server, error) {
	entries, err := c.search(ctx, name, 0, true)
	if err != nil {
		return nil, err
	}
	var newest *Server
	for i := range entries {
		if entries[i].Server.Name != name {
			continue
		}
		if entries[i].isLatest() {
			return &entries[i].Server, nil
		}
		newest = &entries[i].Server
	}
	if newest != nil {
		return newest, nil
	}
	return nil, fmt.Errorf("resolving %s: no such server in the MCP Registry", name)
}

// Publish records a server.json under the caller's namespace. token is the
// bearer from the GitHub OAuth namespace proof. A rejected token is an
// error, never a silent no-op.
func (c *Client) Publish(ctx context.Context, s *Server, token string) error {
	if token == "" {
		return fmt.Errorf("publishing %s: no MCP Registry token; run 'mcpvessel login mcp-registry' first", s.Name)
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
		return fmt.Errorf("publishing %s: token rejected; run 'mcpvessel login mcp-registry' again", s.Name)
	case http.StatusForbidden:
		return fmt.Errorf("publishing %s: token cannot publish this namespace", s.Name)
	default:
		body := snippet(resp.Body)
		// The registry refuses re-publishing an existing version; surface the
		// remedy instead of its raw JSON.
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(body, "duplicate version") {
			return fmt.Errorf("publishing %s: version %s is already in the MCP Registry; bump the version, push, and register again", s.Name, s.Version)
		}
		return fmt.Errorf("publishing %s: registry returned %s: %s", s.Name, resp.Status, body)
	}
}

// get issues a GET against an API path and decodes the JSON body into out,
// retrying a stalled or failed attempt. A GET is idempotent, so retrying is
// safe; nothing that writes goes through here.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	var err error
	for attempt := range readAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(readBackoff):
			}
		}
		if err = c.getOnce(ctx, path, q, out); err == nil {
			return nil
		}
		// A registry that answered is a registry that has made up its mind: a 404
		// or a 500 will say the same thing next time. Only a dead or stalled
		// connection is worth another attempt.
		if !isRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", readAttempts, err)
}

// isRetryable reports whether an error is a transport failure rather than a
// verdict from the registry. Timeouts and dropped connections surface as
// *url.Error; a status code does not.
func isRetryable(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func (c *Client) getOnce(ctx context.Context, path string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
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
	// Bound the success body: a hostile or misconfigured registry must not be
	// able to OOM the CLI with a multi-gigabyte JSON response.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRegistryBody)).Decode(out); err != nil {
		return fmt.Errorf("decoding registry response: %w", err)
	}
	return nil
}

// maxRegistryBody caps a registry success response so a hostile registry
// cannot exhaust CLI memory. A server-list page is far smaller than this.
const maxRegistryBody = 32 << 20

// snippet reads a bounded prefix of an error body so a misbehaving registry
// cannot flood the terminal.
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
