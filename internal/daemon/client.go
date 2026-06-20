package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Client talks to a running daemon over its Unix socket. The CLI commands that
// need the daemon (ps, logs, stop) are thin wrappers over it.
type Client struct {
	http *http.Client
}

// Dial returns a client for the daemon at socketPath. It does not connect yet;
// the first request does, and a connection-refused there means no daemon is
// running.
func Dial(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				// The URL host is a placeholder; every request dials the socket.
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Version returns the running daemon's version, the cheapest call to confirm a
// daemon is up and answering.
func (c *Client) Version(ctx context.Context) (string, error) {
	var body struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/version", &body); err != nil {
		return "", err
	}
	return body.Version, nil
}

// ListRuns returns the runs the daemon is tracking, the data behind ps.
func (c *Client) ListRuns(ctx context.Context) ([]RunInfo, error) {
	var body struct {
		Runs []RunInfo `json:"runs"`
	}
	if err := c.get(ctx, "/runs", &body); err != nil {
		return nil, err
	}
	return body.Runs, nil
}

// StartRun asks the daemon to boot and hold an agent, returning the run id the
// held run is tracked under.
func (c *Client) StartRun(ctx context.Context, ref string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/runs", map[string]string{"ref": ref}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// CallRun dispatches one tool call to a held run and returns its text result.
func (c *Client) CallRun(ctx context.Context, id, tool string, args map[string]any) (string, error) {
	var out struct {
		Result string `json:"result"`
	}
	if err := c.post(ctx, "/runs/"+id+"/call", map[string]any{"tool": tool, "args": args}, &out); err != nil {
		return "", err
	}
	return out.Result, nil
}

// SetBudget changes a held run's LLM budget, in micro-USD.
func (c *Client) SetBudget(ctx context.Context, id string, microUSD int64) error {
	return c.post(ctx, "/runs/"+id+"/budget", map[string]int64{"micro_usd": microUSD}, nil)
}

// StopRun releases a held run.
func (c *Client) StopRun(ctx context.Context, id string) error {
	return c.post(ctx, "/runs/"+id+"/stop", nil, nil)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("contacting the daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon %s: %s", path, errorBody(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding daemon response: %w", err)
	}
	return nil
}

// post sends a JSON body (or none) and decodes a JSON response into out when
// one is expected. A 200 with out decodes; a 204 (stop) carries no body.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("contacting the daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("daemon %s: %s", path, errorBody(resp))
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// errorBody extracts the JSON error message from a non-200 response, falling
// back to the status text when the body is not the expected shape.
func errorBody(resp *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Error != "" {
		return body.Error
	}
	return resp.Status
}
