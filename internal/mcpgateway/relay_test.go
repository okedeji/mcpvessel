package mcpgateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRewriteInitialize(t *testing.T) {
	// An initialize request gains the elicitation capability.
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"roots":{}}}}`
	out := rewriteInitialize([]byte(in))
	if !hasElicitationCap(t, out) {
		t.Errorf("initialize was not given the elicitation capability: %s", out)
	}

	// A request with no capabilities object still gains one.
	out = rewriteInitialize([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if !hasElicitationCap(t, out) {
		t.Errorf("initialize with empty params was not given the capability: %s", out)
	}

	// Anything that is not an initialize request is returned byte-for-byte.
	other := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if got := string(rewriteInitialize([]byte(other))); got != other {
		t.Errorf("non-initialize body was rewritten: %s", got)
	}
}

func hasElicitationCap(t *testing.T, body []byte) bool {
	t.Helper()
	var msg struct {
		Params struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	_, ok := msg.Params.Capabilities["elicitation"]
	return ok
}

func TestStripDeniedTools(t *testing.T) {
	deny := denySet([]string{"delete_all"})
	list := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search"},{"name":"delete_all"}]}}`
	out, changed := stripDeniedTools([]byte(list), deny)
	if !changed {
		t.Fatal("expected the denied tool to be stripped")
	}
	if strings.Contains(string(out), "delete_all") {
		t.Errorf("denied tool still present: %s", out)
	}
	if !strings.Contains(string(out), "search") {
		t.Errorf("allowed tool was dropped: %s", out)
	}

	// A result with no denied tools is left untouched.
	if _, changed := stripDeniedTools([]byte(`{"result":{"tools":[{"name":"search"}]}}`), deny); changed {
		t.Error("a list with no denied tools should be unchanged")
	}
	// A non-list message is left untouched.
	if _, changed := stripDeniedTools([]byte(`{"result":{"ok":true}}`), deny); changed {
		t.Error("a non-list result should be unchanged")
	}
	// An empty deny set strips nothing.
	if _, changed := stripDeniedTools([]byte(list), nil); changed {
		t.Error("an empty deny set should strip nothing")
	}
}

func TestIsElicitationCreate(t *testing.T) {
	if !isElicitationCreate([]byte(`{"jsonrpc":"2.0","id":1,"method":"elicitation/create","params":{}}`)) {
		t.Error("did not recognize an elicitation/create request")
	}
	if isElicitationCreate([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) {
		t.Error("a result was mistaken for an elicitation request")
	}
}

func TestElicitResponse(t *testing.T) {
	ok := elicitResponse(json.RawMessage(`5`), ElicitAnswer{Action: "accept", Content: map[string]any{"x": 1}}, true)
	if !strings.Contains(string(ok), `"result"`) || !strings.Contains(string(ok), `"id":5`) {
		t.Errorf("accepted answer should carry a result and the id: %s", ok)
	}
	fail := elicitResponse(json.RawMessage(`5`), ElicitAnswer{}, false)
	if !strings.Contains(string(fail), `"error"`) {
		t.Errorf("an unreached operator should produce a JSON-RPC error: %s", fail)
	}
}

// TestGateway_ElicitRoundTrip proves the control-stream express lane: Elicit
// emits a question and returns the daemon's answer correlated by id.
func TestGateway_ElicitRoundTrip(t *testing.T) {
	g := New(Config{Edges: map[string]Edge{"sub": {Target: "http://x/mcp"}}})
	gwConn, daemonConn := net.Pipe()
	t.Cleanup(func() { _ = gwConn.Close(); _ = daemonConn.Close() })
	go func() { _ = g.ServeControl(gwConn) }()

	dec := json.NewDecoder(daemonConn)
	enc := json.NewEncoder(daemonConn)
	skipResync(t, dec)

	go func() {
		var q ControlMessage
		if dec.Decode(&q) == nil && q.Type == MsgElicit {
			_ = enc.Encode(ControlMessage{Type: MsgElicitResult, ID: q.ID, OK: true,
				Answer: &ElicitAnswer{Action: "accept", Content: map[string]any{"env": "prod"}}})
		}
	}()

	ans, ok := g.Elicit(context.Background(), "sub", ElicitQuestion{Message: "prod or staging?"})
	if !ok {
		t.Fatal("Elicit returned not-ok")
	}
	if ans.Action != "accept" || ans.Content["env"] != "prod" {
		t.Errorf("answer = %+v, want accept/prod", ans)
	}
}

// TestGateway_ElicitFailsClosedOnDisconnect proves a question whose answer can
// no longer arrive fails closed rather than hanging until the timeout.
func TestGateway_ElicitFailsClosedOnDisconnect(t *testing.T) {
	g := New(Config{Edges: map[string]Edge{"sub": {Target: "http://x/mcp"}}})
	gwConn, daemonConn := net.Pipe()
	go func() { _ = g.ServeControl(gwConn) }()

	dec := json.NewDecoder(daemonConn)
	skipResync(t, dec)

	done := make(chan bool, 1)
	go func() {
		_, ok := g.Elicit(context.Background(), "sub", ElicitQuestion{Message: "x"})
		done <- ok
	}()

	// Read the question so the waiter is registered, then drop the stream.
	var q ControlMessage
	if err := dec.Decode(&q); err != nil {
		t.Fatalf("reading the elicit: %v", err)
	}
	_ = daemonConn.Close()
	_ = gwConn.Close()

	select {
	case ok := <-done:
		if ok {
			t.Error("Elicit should fail closed when the control stream drops")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Elicit did not return after the stream dropped")
	}
}

// TestGateway_BubblesSubAgentElicitationToOperator is the end-to-end proof: a
// sub-agent asks a question mid-call, a plain parent client (no elicitation
// support of its own) calls it through the gateway, and the gateway makes the
// sub-agent able to ask, intercepts the question, routes it to the operator over
// the control stream, and posts the answer back so the call finishes.
func TestGateway_BubblesSubAgentElicitationToOperator(t *testing.T) {
	sub := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sub", Version: "0"}, nil)
	mcpsdk.AddTool(sub, &mcpsdk.Tool{Name: "deploy"},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
			res, err := req.Session.Elicit(ctx, &mcpsdk.ElicitParams{Message: "prod or staging?"})
			if err != nil {
				return nil, struct{}{}, err
			}
			env, _ := res.Content["env"].(string)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "deploying to " + env}}}, struct{}{}, nil
		})
	subSrv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return sub }, nil))
	defer subSrv.Close()

	g := New(Config{Edges: map[string]Edge{"sub": {Target: subSrv.URL + "/mcp"}}})
	gwSrv := httptest.NewServer(g.Handler())
	defer gwSrv.Close()

	gwConn, daemonConn := net.Pipe()
	t.Cleanup(func() { _ = gwConn.Close(); _ = daemonConn.Close() })
	go func() { _ = g.ServeControl(gwConn) }()
	go fakeDaemon(daemonConn, ElicitAnswer{Action: "accept", Content: map[string]any{"env": "staging"}})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	parent := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "parent", Version: "0"}, nil)
	psess, err := parent.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: gwSrv.URL + "/sub/mcp"}, nil)
	if err != nil {
		t.Fatalf("parent connect: %v", err)
	}
	defer func() { _ = psess.Close() }()

	res, err := psess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "deploy"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error: %v", res.Content)
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcpsdk.TextContent); ok {
			text = tc.Text
		}
	}
	if !strings.Contains(text, "staging") {
		t.Errorf("result = %q, want the operator's answer folded in", text)
	}
}

// TestGateway_StripsDeniedToolsFromList proves the response-side deny strip over
// a real listing: a denied tool never appears in the tools/list the parent sees.
func TestGateway_StripsDeniedToolsFromList(t *testing.T) {
	sub := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sub", Version: "0"}, nil)
	mcpsdk.AddTool(sub, &mcpsdk.Tool{Name: "search"}, okTool)
	mcpsdk.AddTool(sub, &mcpsdk.Tool{Name: "delete_all"}, okTool)
	subSrv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return sub }, nil))
	defer subSrv.Close()

	g := New(Config{Edges: map[string]Edge{"sub": {Target: subSrv.URL + "/mcp", Deny: []string{"delete_all"}}}})
	gwSrv := httptest.NewServer(g.Handler())
	defer gwSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "parent", Version: "0"}, nil)
	psess, err := parent.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: gwSrv.URL + "/sub/mcp"}, nil)
	if err != nil {
		t.Fatalf("parent connect: %v", err)
	}
	defer func() { _ = psess.Close() }()

	res, err := psess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "delete_all" {
			t.Errorf("denied tool appeared in the parent's tools/list")
		}
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "search" {
		t.Errorf("tools = %v, want only search", res.Tools)
	}
}

func okTool(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, struct{}{}, nil
}

// fakeDaemon answers every question the gateway raises with a fixed answer, the
// operator's stand-in for the integration tests.
func fakeDaemon(conn io.ReadWriteCloser, answer ElicitAnswer) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var m ControlMessage
		if dec.Decode(&m) != nil {
			return
		}
		if m.Type == MsgElicit {
			a := answer
			_ = enc.Encode(ControlMessage{Type: MsgElicitResult, ID: m.ID, OK: true, Answer: &a})
		}
	}
}

func skipResync(t *testing.T, dec *json.Decoder) {
	t.Helper()
	var m ControlMessage
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("reading the opening resync: %v", err)
	}
	if m.Type != MsgResync {
		t.Fatalf("first control message = %s, want resync", m.Type)
	}
}
