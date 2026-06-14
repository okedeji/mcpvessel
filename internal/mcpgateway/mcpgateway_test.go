package mcpgateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeniedCall(t *testing.T) {
	deny := denySet([]string{"delete_all"})
	cases := []struct {
		name       string
		body       string
		wantTool   string
		wantDenied bool
	}{
		{"denied call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all"}}`, "delete_all", true},
		{"allowed call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`, "", false},
		{"tools list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "", false},
		{"garbage", `not json`, "", false},
		// A batch must not smuggle a denied call past the single-object check.
		{"batch with denied", `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all"}}]`, "delete_all", true},
		{"batch denied behind allowed", `[{"method":"tools/call","params":{"name":"search"}},{"method":"tools/call","params":{"name":"delete_all"}}]`, "delete_all", true},
		{"batch all allowed", `[{"method":"tools/call","params":{"name":"search"}}]`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, denied := deniedCall([]byte(c.body), deny)
			if denied != c.wantDenied || tool != c.wantTool {
				t.Errorf("deniedCall = (%q, %v), want (%q, %v)", tool, denied, c.wantTool, c.wantDenied)
			}
		})
	}
	if _, denied := deniedCall([]byte(`{"method":"tools/call","params":{"name":"x"}}`), nil); denied {
		t.Error("an empty deny set should deny nothing")
	}
}

func TestHandler_ForwardsAllowedAndRejectsDenied(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer upstream.Close()

	gw := httptest.NewServer(Handler(Config{Edges: map[string]Edge{
		"web": {Target: upstream.URL + "/mcp", Deny: []string{"delete_all"}},
	}}))
	defer gw.Close()

	// An allowed tools/call is forwarded and its response returned.
	resp := postJSON(t, gw.URL+"/web/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`)
	if hits != 1 {
		t.Errorf("allowed call not forwarded (upstream hits = %d)", hits)
	}
	if !strings.Contains(resp, `"ok":true`) {
		t.Errorf("upstream response not returned: %s", resp)
	}

	// A denied tools/call never reaches upstream; the gateway returns a
	// JSON-RPC error echoing the request id.
	resp = postJSON(t, gw.URL+"/web/mcp", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"delete_all"}}`)
	if hits != 1 {
		t.Errorf("denied call reached upstream (upstream hits = %d)", hits)
	}
	if !strings.Contains(resp, "-32003") || !strings.Contains(resp, "delete_all") {
		t.Errorf("expected a deny error naming the tool, got: %s", resp)
	}
	if !strings.Contains(resp, `"id":7`) {
		t.Errorf("deny error should echo request id 7: %s", resp)
	}
}

func TestHandler_BannedEdgeRejectsEverything(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer upstream.Close()

	gw := httptest.NewServer(Handler(Config{Edges: map[string]Edge{
		"weird": {Target: upstream.URL + "/mcp", Banned: true},
	}}))
	defer gw.Close()

	// Even the initialize handshake is rejected, and nothing reaches the
	// (in a real run, never-started) target.
	resp := postJSON(t, gw.URL+"/weird/mcp", `{"jsonrpc":"2.0","id":3,"method":"initialize"}`)
	if hits != 0 {
		t.Errorf("a banned edge forwarded to upstream (hits = %d)", hits)
	}
	if !strings.Contains(resp, "-32004") || !strings.Contains(resp, "banned") {
		t.Errorf("expected a banned error, got: %s", resp)
	}
	if !strings.Contains(resp, `"id":3`) {
		t.Errorf("banned error should echo request id 3: %s", resp)
	}

	// A tools/call is rejected the same way.
	resp = postJSON(t, gw.URL+"/weird/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"anything"}}`)
	if hits != 0 {
		t.Errorf("a banned tools/call forwarded to upstream (hits = %d)", hits)
	}
	if !strings.Contains(resp, "-32004") {
		t.Errorf("expected a banned error for tools/call, got: %s", resp)
	}
}

func TestHandler_UnknownEdgeIs404(t *testing.T) {
	gw := httptest.NewServer(Handler(Config{Edges: map[string]Edge{}}))
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/nope/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown edge status = %d, want 404", resp.StatusCode)
	}
}

func postJSON(t *testing.T, url, body string) string {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
