package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/okedeji/mcpvessel/internal/identity"
	"github.com/okedeji/mcpvessel/internal/llmgateway"
	"github.com/okedeji/mcpvessel/internal/runtime"
	"github.com/okedeji/mcpvessel/internal/telemetry"
)

// Client talks to a running daemon over its Unix socket.
type Client struct {
	http *http.Client

	// staleOnce gates the stale-daemon warning to once per process; every
	// response carries the identity headers, one warning is enough.
	staleOnce sync.Once
}

// StaleWarnWriter receives the one-line warning when the daemon's build no
// longer matches the binary on disk. Package-level so every command inherits
// the check without wiring; tests may point it elsewhere.
var StaleWarnWriter io.Writer = os.Stderr

// checkStale compares the daemon's stamped identity against this process's
// version and the daemon's binary on disk. A daemon spawned before a rebuild
// or upgrade keeps running old orchestration code while looking healthy;
// this is the only place that mismatch becomes visible. Headers absent (an
// older daemon) skip the check rather than false-positive.
func (c *Client) checkStale(h http.Header) {
	c.staleOnce.Do(func() {
		version := h.Get(headerVersion)
		if version == "" {
			return
		}
		stale := version != identity.Version
		if !stale {
			exe := h.Get(headerBinary)
			mtime, err := strconv.ParseInt(h.Get(headerBinaryMtime), 10, 64)
			if exe == "" || err != nil {
				return
			}
			// A stat error means the daemon's binary is gone from disk
			// (an upgrade unlinked it): stale by definition.
			info, statErr := os.Stat(exe)
			stale = statErr != nil || info.ModTime().Unix() != mtime
		}
		if stale {
			_, _ = fmt.Fprintln(StaleWarnWriter, "warning: the daemon is running a stale mcpvessel build; restart it with 'mcpvessel daemon stop && mcpvessel init'")
		}
	})
}

// Unreachable wraps a failure to reach the daemon at all, distinct from an
// error the daemon itself returned.
type Unreachable struct{ Err error }

func (u *Unreachable) Error() string { return u.Err.Error() }
func (u *Unreachable) Unwrap() error { return u.Err }

// Dial returns a client for the daemon at socketPath. It does not connect
// until the first request.
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

// Version returns the running daemon's version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var body struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/version", &body); err != nil {
		return "", err
	}
	return body.Version, nil
}

// ListRuns returns the runs the daemon is tracking.
func (c *Client) ListRuns(ctx context.Context) ([]RunInfo, error) {
	var body struct {
		Runs []RunInfo `json:"runs"`
	}
	if err := c.get(ctx, "/runs", &body); err != nil {
		return nil, err
	}
	return body.Runs, nil
}

// RunUsage is what a completed run cost and how long its tool call took.
// CallDuration times the call alone, excluding boot.
type RunUsage struct {
	RunID        string
	CostMicroUSD int64
	CallDuration time.Duration
}

// RunOnce runs an agent to completion through the daemon, streaming its logs
// to logs and returning the final tool result. A failure to reach the daemon
// comes back as *Unreachable.
func (c *Client) RunOnce(ctx context.Context, req RunRequest, logs io.Writer) (string, error) {
	result, _, err := c.runStream(ctx, req, logs)
	return result, err
}

// RunOnceUsage is RunOnce plus the run's cost and call duration. Usage is
// populated even when the run returns an error.
func (c *Client) RunOnceUsage(ctx context.Context, req RunRequest, logs io.Writer) (string, RunUsage, error) {
	return c.runStream(ctx, req, logs)
}

// RecordRun runs an agent with replay recording on, returning the run id
// needed to fetch the .replay afterward.
func (c *Client) RecordRun(ctx context.Context, req RunRequest, logs io.Writer) (runID, result string, err error) {
	req.Record = true
	result, usage, err := c.runStream(ctx, req, logs)
	return usage.RunID, result, err
}

// runStream drives one /run request, streaming logs and returning the final
// result plus usage.
func (c *Client) runStream(ctx context.Context, req RunRequest, logs io.Writer) (result string, usage RunUsage, err error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", usage, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/run", bytes.NewReader(body))
	if err != nil {
		return "", usage, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", usage, &Unreachable{err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", usage, fmt.Errorf("daemon /run: %s", errorBody(resp))
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var f runFrame
		if derr := dec.Decode(&f); derr != nil {
			if derr == io.EOF {
				return result, usage, nil
			}
			return result, usage, fmt.Errorf("reading run stream: %w", derr)
		}
		switch f.Type {
		case "run_id":
			usage.RunID = f.Data
		case "log":
			_, _ = io.WriteString(logs, f.Data)
		case "result":
			result = f.Data
			usage.CostMicroUSD = f.CostMicroUSD
			usage.CallDuration = time.Duration(f.CallMS) * time.Millisecond
		case "error":
			usage.CostMicroUSD = f.CostMicroUSD
			usage.CallDuration = time.Duration(f.CallMS) * time.Millisecond
			return result, usage, fmt.Errorf("%s", f.Data)
		}
	}
}

// FetchReplay returns a recorded run's .replay artifact bytes.
func (c *Client) FetchReplay(ctx context.Context, id string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/runs/"+id+"/replay", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting the daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon /runs/%s/replay: %s", id, errorBody(resp))
	}
	return io.ReadAll(resp.Body)
}

// Logs streams a run's logs to w. With follow it tails a live run until the
// run ends; without it, it writes the log to date and returns.
func (c *Client) Logs(ctx context.Context, id string, follow bool, w io.Writer) error {
	path := "/runs/" + id + "/logs"
	if follow {
		path += "?follow=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Unreachable{err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon %s: %s", path, errorBody(resp))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Events streams the daemon's lifecycle feed, calling onEvent for each event
// until ctx is cancelled or the daemon closes the stream.
func (c *Client) Events(ctx context.Context, onEvent func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/events", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Unreachable{err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon /events: %s", errorBody(resp))
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading event stream: %w", err)
		}
		onEvent(e)
	}
}

// Stats returns a live snapshot of every cage's resource usage.
func (c *Client) Stats(ctx context.Context) ([]runtime.CageStat, error) {
	var body struct {
		Cages []runtime.CageStat `json:"cages"`
	}
	if err := c.get(ctx, "/stats", &body); err != nil {
		return nil, err
	}
	return body.Cages, nil
}

// Trace returns a finished run's trace. It errors when the run made no LLM
// call (no trace was built) or is unknown.
func (c *Client) Trace(ctx context.Context, id string) (*telemetry.Trace, error) {
	var tr telemetry.Trace
	if err := c.get(ctx, "/runs/"+id+"/trace", &tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

// Spend returns a live run's current LLM spend. It errors when the run is not
// reasoning or no longer running.
func (c *Client) Spend(ctx context.Context, id string) (llmgateway.SpendReport, error) {
	var report llmgateway.SpendReport
	if err := c.get(ctx, "/runs/"+id+"/spend", &report); err != nil {
		return llmgateway.SpendReport{}, err
	}
	return report, nil
}

// StartRun asks the daemon to boot and hold an agent, returning its run id.
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

// ServedAgent is one endpoint the front door opened: its /agents/ address and
// the public tools it exposes.
type ServedAgent struct {
	Address string   `json:"address"`
	Tools   []string `json:"tools"`
}

// ServedFlat is the merged endpoint: one URL advertising every served
// bundle's public tools at once.
type ServedFlat struct {
	Path  string   `json:"path"`
	Tools []string `json:"tools"`
}

// ServeResult is the front door the daemon opened for a serve request, plus
// any boot-time warnings for the operator.
type ServeResult struct {
	Listen   string        `json:"listen"`
	Flat     ServedFlat    `json:"flat"`
	Agents   []ServedAgent `json:"agents"`
	Warnings []string      `json:"warnings,omitempty"`
}

// ServeTarget is one bundle to serve: a daemon-resolvable ref and an
// optional name overriding the root agent's address (a source directory
// resolves to a content hash, whose prefix would make a poor address).
type ServeTarget struct {
	Ref  string `json:"ref"`
	Name string `json:"name,omitempty"`
}

// Serve asks the daemon to register the bundles' exposed sets and open one
// MCP front door bound to listen.
func (c *Client) Serve(ctx context.Context, targets []ServeTarget, listen string, expose, noExpose []string, observe bool, egress map[string][]string, env, secrets map[string]string) (ServeResult, error) {
	var out ServeResult
	err := c.post(ctx, "/serve", map[string]any{
		"bundles":   targets,
		"listen":    listen,
		"expose":    expose,
		"no_expose": noExpose,
		"observe":   observe,
		"egress":    egress,
		"env":       env,
		"secrets":   secrets,
	}, &out)
	return out, err
}

// StopRun releases a held run.
func (c *Client) StopRun(ctx context.Context, id string) error {
	return c.post(ctx, "/runs/"+id+"/stop", nil, nil)
}

// Shutdown asks the daemon to stop. The daemon acks before going down, but the
// connection may still race the shutdown; a transport error here does not mean
// the request was lost.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.post(ctx, "/shutdown", nil, nil)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Unreachable{fmt.Errorf("contacting the daemon: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	c.checkStale(resp.Header)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon %s: %s", path, errorBody(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding daemon response: %w", err)
	}
	return nil
}

// post sends a JSON body (or none); out decodes a 200 response, a 204 carries
// no body.
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
		return &Unreachable{fmt.Errorf("contacting the daemon: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	c.checkStale(resp.Header)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("daemon %s: %s", path, errorBody(resp))
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// errorBody extracts the JSON error message from a non-200 response, falling
// back to the status text.
func errorBody(resp *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Error != "" {
		return body.Error
	}
	return resp.Status
}
