package mcpgateway

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// daemonEnd drives the control stream the way the real daemon does: it reads one
// activation request and answers it with the given verdict, returning the edge
// it saw so a test can assert the gateway asked for the right one.
func daemonEnd(t *testing.T, conn net.Conn, ok bool) string {
	t.Helper()
	var req controlMsg
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		t.Fatalf("reading activate: %v", err)
	}
	if req.Type != "activate" {
		t.Fatalf("got %q, want an activate message", req.Type)
	}
	if err := json.NewEncoder(conn).Encode(controlMsg{Type: "activated", Edge: req.Edge, OK: ok}); err != nil {
		t.Fatalf("writing activated: %v", err)
	}
	return req.Edge
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

	// The call to an inactive edge blocks until the daemon activates it, so the
	// daemon side and the call run concurrently.
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

	// A second call to the now-live edge proxies directly, no activation needed.
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

	// Read the activate so the call is genuinely blocked, then drop the stream
	// without answering: the blocked call must fail closed, not hang.
	var req controlMsg
	if err := json.NewDecoder(daemon).Decode(&req); err != nil {
		t.Fatalf("reading activate: %v", err)
	}
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
