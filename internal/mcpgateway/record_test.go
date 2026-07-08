package mcpgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGateway_RecordsSubAgentCall drives a real tools/call through the
// gateway and checks the metadata hook and the payload capture.
func TestGateway_RecordsSubAgentCall(t *testing.T) {
	sub := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sub", Version: "0"}, nil)
	mcpsdk.AddTool(sub, &mcpsdk.Tool{Name: "search"}, okTool)
	subSrv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return sub }, nil))
	defer subSrv.Close()

	g := New(Config{Edges: map[string]Edge{"sub": {Target: subSrv.URL + "/mcp"}}, Record: true})
	var calls []SubCallEvent
	var records []SubCallRecord
	g.SetHooks(Hooks{
		Call:    func(e SubCallEvent) { calls = append(calls, e) },
		Payload: func(r SubCallRecord) { records = append(records, r) },
	})
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

	if _, err := psess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "search", Arguments: map[string]any{"q": "needle"}}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// Only the tools/call is recorded, not the handshake or tools/list.
	if len(calls) != 1 || calls[0].Tool != "search" || calls[0].Edge != "sub" {
		t.Fatalf("call events = %+v, want one search call on edge sub", calls)
	}
	if len(records) != 1 {
		t.Fatalf("payload records = %d, want 1", len(records))
	}
	if !strings.Contains(string(records[0].Args), "needle") {
		t.Errorf("args not captured: %s", records[0].Args)
	}
	if len(records[0].Response) == 0 {
		t.Error("response not captured")
	}
}
