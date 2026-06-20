package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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
