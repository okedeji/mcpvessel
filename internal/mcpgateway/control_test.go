package mcpgateway

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// flakyRoundTripper fails its first call with failFirst (if set) and
// succeeds after.
type flakyRoundTripper struct {
	mu        sync.Mutex
	calls     int
	failFirst error
}

func (f *flakyRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n == 1 && f.failFirst != nil {
		return nil, f.failFirst
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
}

func (f *flakyRoundTripper) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ackActivations answers every activate request with an OK verdict.
func ackActivations(conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var m ControlMessage
		if dec.Decode(&m) != nil {
			return
		}
		if m.Type == MsgActivate {
			_ = enc.Encode(ControlMessage{Type: MsgActivated, Edge: m.Edge, OK: true})
		}
	}
}

func TestRetryTransport_ReactivatesAndRetriesOnUnreachable(t *testing.T) {
	gw := New(Config{Edges: map[string]Edge{"e": {Target: "http://nope:8000/mcp", Inactive: true}}})
	gwEnd, daemon := net.Pipe()
	go func() { _ = gw.ServeControl(gwEnd) }()
	go ackActivations(daemon)
	defer func() { _ = daemon.Close() }()

	flaky := &flakyRoundTripper{failFirst: syscall.ECONNREFUSED}
	rt := &retryTransport{gw: gw, edge: "e", base: flaky}
	req, _ := http.NewRequest(http.MethodPost, "http://nope:8000/mcp", strings.NewReader("body"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("body")), nil }

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip after reactivation: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after retry", resp.StatusCode)
	}
	if flaky.count() != 2 {
		t.Errorf("base called %d times, want 2 (one retry)", flaky.count())
	}
}

func TestRetryTransport_DoesNotRetryOtherErrors(t *testing.T) {
	gw := New(Config{Edges: map[string]Edge{"e": {Target: "http://nope:8000/mcp"}}})
	flaky := &flakyRoundTripper{failFirst: errors.New("upstream rejected the request")}
	rt := &retryTransport{gw: gw, edge: "e", base: flaky}
	req, _ := http.NewRequest(http.MethodPost, "http://nope:8000/mcp", strings.NewReader("body"))

	if _, err := rt.RoundTrip(req); err == nil {
		t.Error("a non-connection error should be returned, not retried")
	}
	if flaky.count() != 1 {
		t.Errorf("base called %d times, want 1 (no retry)", flaky.count())
	}
}

// readActivate reads to the first activate, skipping the opening resync and
// any pin/unpin events.
func readActivate(t *testing.T, dec *json.Decoder) string {
	t.Helper()
	for {
		var m ControlMessage
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("reading control stream: %v", err)
		}
		if m.Type == MsgActivate {
			return m.Edge
		}
	}
}

// daemonEnd reads to the activation request, answers with the given verdict,
// and returns the edge it saw.
func daemonEnd(t *testing.T, conn net.Conn, ok bool) string {
	t.Helper()
	edge := readActivate(t, json.NewDecoder(conn))
	if err := json.NewEncoder(conn).Encode(ControlMessage{Type: MsgActivated, Edge: edge, OK: ok}); err != nil {
		t.Fatalf("writing activated: %v", err)
	}
	return edge
}

func TestGateway_ActivatesInactiveEdgeThenProxies(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer upstream.Close()

	gw := New(Config{Edges: map[string]Edge{
		"web": {Target: upstream.URL + "/mcp", Inactive: true},
	}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	gwEnd, daemon := net.Pipe()
	go func() { _ = gw.ServeControl(gwEnd) }()
	defer func() { _ = daemon.Close() }()

	// The call blocks until activation, so it runs concurrently with the
	// daemon side.
	type result struct{ body string }
	resCh := make(chan result, 1)
	go func() {
		resCh <- result{postJSON(t, srv.URL+"/web/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`)}
	}()

	if edge := daemonEnd(t, daemon, true); edge != "web" {
		t.Fatalf("activated %q, want web", edge)
	}

	select {
	case res := <-resCh:
		if hits != 1 {
			t.Errorf("activated call not forwarded (upstream hits = %d)", hits)
		}
		if !strings.Contains(res.body, `"ok":true`) {
			t.Errorf("upstream response not returned: %s", res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after activation")
	}

	// A second call to the now-live edge proxies directly.
	_ = postJSON(t, srv.URL+"/web/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`)
	if hits != 2 {
		t.Errorf("second call to a live edge not forwarded (upstream hits = %d)", hits)
	}
}

func TestGateway_FailedActivationFailsClosed(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer upstream.Close()

	gw := New(Config{Edges: map[string]Edge{
		"web": {Target: upstream.URL + "/mcp", Inactive: true},
	}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	gwEnd, daemon := net.Pipe()
	go func() { _ = gw.ServeControl(gwEnd) }()
	defer func() { _ = daemon.Close() }()

	resCh := make(chan string, 1)
	go func() {
		resCh <- postJSON(t, srv.URL+"/web/mcp", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search"}}`)
	}()

	daemonEnd(t, daemon, false)

	select {
	case body := <-resCh:
		if hits != 0 {
			t.Errorf("a failed activation still forwarded (upstream hits = %d)", hits)
		}
		if !strings.Contains(body, "-32002") {
			t.Errorf("expected an activation-failed error, got: %s", body)
		}
		if !strings.Contains(body, `"id":9`) {
			t.Errorf("activation-failed error should echo request id 9: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed-activation call did not return")
	}
}

// daemonEndAddr answers the activate with an address, as the real daemon
// hands over the booted cage's IP.
func daemonEndAddr(t *testing.T, conn net.Conn, addr string) string {
	t.Helper()
	edge := readActivate(t, json.NewDecoder(conn))
	if err := json.NewEncoder(conn).Encode(ControlMessage{Type: MsgActivated, Edge: edge, OK: true, Addr: addr}); err != nil {
		t.Fatalf("writing activated: %v", err)
	}
	return edge
}

func TestGateway_ForwardsToActivatedAddress(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer upstream.Close()

	// The configured target is dead; only the activation address can make the
	// forward land, since the gateway's /etc/hosts cannot name a cage that
	// started after it.
	gw := New(Config{Edges: map[string]Edge{
		"web": {Target: "http://127.0.0.1:1/mcp", Inactive: true},
	}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	gwEnd, daemon := net.Pipe()
	go func() { _ = gw.ServeControl(gwEnd) }()
	defer func() { _ = daemon.Close() }()

	resCh := make(chan string, 1)
	go func() {
		resCh <- postJSON(t, srv.URL+"/web/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`)
	}()

	daemonEndAddr(t, daemon, upstream.URL+"/mcp")

	select {
	case res := <-resCh:
		if hits != 1 {
			t.Errorf("forward did not reach the supplied address (upstream hits = %d)", hits)
		}
		if !strings.Contains(res, `"ok":true`) {
			t.Errorf("upstream response not returned: %s", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after activation")
	}
}

func TestGateway_DeactivateForcesReactivation(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer upstream.Close()

	gw := New(Config{Edges: map[string]Edge{
		"web": {Target: upstream.URL + "/mcp", Inactive: true},
	}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	gwEnd, daemon := net.Pipe()
	go func() { _ = gw.ServeControl(gwEnd) }()
	defer func() { _ = daemon.Close() }()

	// One writer and one reader own the daemon conn so the test never races
	// two encoders on the pipe.
	out := make(chan ControlMessage, 8)
	activates := make(chan string, 4)
	go func() {
		enc := json.NewEncoder(daemon)
		for m := range out {
			_ = enc.Encode(m)
		}
	}()
	go func() {
		dec := json.NewDecoder(daemon)
		for {
			var m ControlMessage
			if dec.Decode(&m) != nil {
				return
			}
			if m.Type == MsgActivate {
				out <- ControlMessage{Type: MsgActivated, Edge: m.Edge, OK: true, Addr: upstream.URL + "/mcp"}
				activates <- m.Edge
			}
		}
	}()

	done := make(chan struct{}, 2)
	call := func(id int) {
		_ = postJSON(t, srv.URL+"/web/mcp", `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"method":"tools/call","params":{"name":"x"}}`)
		done <- struct{}{}
	}

	go call(1)
	waitActivate(t, activates)
	<-done

	// After a deactivate the next call must re-activate rather than proxy to
	// whatever cage next takes the freed address.
	out <- ControlMessage{Type: MsgDeactivate, Edge: "web"}

	go call(2)
	waitActivate(t, activates)
	<-done

	if hits != 2 {
		t.Errorf("expected the deactivated edge to forward again after re-activation, hits = %d", hits)
	}
}

func waitActivate(t *testing.T, activates <-chan string) {
	t.Helper()
	select {
	case <-activates:
	case <-time.After(5 * time.Second):
		t.Fatal("expected an activation request")
	}
}

func TestGateway_ForwardFailureCarriesRequestID(t *testing.T) {
	// Both target and activation address are dead, so the forward lands in
	// the ErrorHandler. Its response must still echo the request id; a null
	// id is unparseable to an MCP client.
	defer func(b time.Duration) { coldStartBudget = b }(coldStartBudget)
	coldStartBudget = 50 * time.Millisecond

	gw := New(Config{Edges: map[string]Edge{
		"web": {Target: "http://127.0.0.1:1/mcp", Inactive: true},
	}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	gwEnd, daemon := net.Pipe()
	go func() { _ = gw.ServeControl(gwEnd) }()
	go ackActivations(daemon)
	defer func() { _ = daemon.Close() }()

	body := postJSON(t, srv.URL+"/web/mcp", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search"}}`)
	if !strings.Contains(body, "-32002") {
		t.Errorf("expected an activation-failed error, got: %s", body)
	}
	if !strings.Contains(body, `"id":7`) {
		t.Errorf("forward-failure error must echo request id 7, got: %s", body)
	}
}

func TestGateway_DisconnectFailsBlockedCallClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	gw := New(Config{Edges: map[string]Edge{
		"web": {Target: upstream.URL + "/mcp", Inactive: true},
	}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	gwEnd, daemon := net.Pipe()
	go func() { _ = gw.ServeControl(gwEnd) }()

	resCh := make(chan string, 1)
	go func() {
		resCh <- postJSON(t, srv.URL+"/web/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search"}}`)
	}()

	// Read to the activate so the call is genuinely blocked, then drop the
	// stream without answering: the call must fail closed, not hang.
	readActivate(t, json.NewDecoder(daemon))
	_ = daemon.Close()

	select {
	case body := <-resCh:
		if !strings.Contains(body, "-32002") {
			t.Errorf("expected an activation-failed error on disconnect, got: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not fail closed after the control stream dropped")
	}
}
