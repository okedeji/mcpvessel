package serve

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
)

// staticAgent wraps a fixed call (and optional elicit binding) as an Agent whose
// resolver returns the same target for every client session, the simple shape
// the front-door tests need without a real instance manager.
func staticAgent(addr string, tools []mcp.Tool, call func(context.Context, string, map[string]any) (string, error), bind func(mcp.ElicitHandler) func()) Agent {
	return Agent{
		Address: addr,
		Tools:   tools,
		Resolve: func(context.Context, string) (Target, func(), error) {
			return Target{Call: call, BindElicit: bind}, func() {}, nil
		},
	}
}

func TestHandler_ListsAndCallsPublicTools(t *testing.T) {
	var called string
	agent := staticAgent("researcher",
		[]mcp.Tool{{Name: "search", Description: "search the web", Schema: map[string]any{"type": "object"}}},
		func(_ context.Context, tool string, args map[string]any) (string, error) {
			called = tool
			return "q=" + args["q"].(string), nil
		}, nil)

	srv := httptest.NewServer(Handler([]Agent{agent}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := mcp.ConnectHTTP(ctx, srv.URL+"/agents/researcher/mcp")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("tools = %v, want one named search", tools)
	}

	out, err := client.CallTool(ctx, "search", map[string]any{"q": "agentic memory"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if called != "search" {
		t.Errorf("dispatched tool = %q, want search", called)
	}
	if !strings.Contains(out, "agentic memory") {
		t.Errorf("result = %q, want it to carry the argument", out)
	}
}

func TestHandler_PrivateToolUnreachable(t *testing.T) {
	agent := staticAgent("researcher",
		[]mcp.Tool{{Name: "search", Schema: map[string]any{"type": "object"}}},
		func(_ context.Context, tool string, _ map[string]any) (string, error) {
			return "should not run for " + tool, nil
		}, nil)

	srv := httptest.NewServer(Handler([]Agent{agent}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := mcp.ConnectHTTP(ctx, srv.URL+"/agents/researcher/mcp")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	// rerank was never registered (it is private), so the front door must reject
	// the call rather than dispatch it into the run.
	if _, err := client.CallTool(ctx, "rerank", nil); err == nil {
		t.Fatal("front door dispatched a call to an unregistered private tool")
	}
}

// askingAgent is an exposed agent whose tool asks the caller a question
// mid-call through its bound answer channel, the way a real agent elicits
// through the front door. bound mirrors what the daemon's session.BindElicit
// does: it installs the answer channel for the call's duration.
func askingAgent() Agent {
	var bound mcp.ElicitHandler
	return staticAgent("asker",
		[]mcp.Tool{{Name: "deploy", Schema: map[string]any{"type": "object"}}},
		func(ctx context.Context, _ string, _ map[string]any) (string, error) {
			res, err := bound(ctx, &mcp.ElicitRequest{Message: "prod or staging?"})
			if err != nil {
				return "", err
			}
			where, _ := res.Content["env"].(string)
			return "deploying to " + where, nil
		},
		func(h mcp.ElicitHandler) func() {
			bound = h
			return func() { bound = nil }
		})
}

// TestHandler_ElicitsThroughCaller is the serve-layer round trip: the agent asks
// a question mid-call, it rides MCP's elicitation back to the calling client,
// and the caller's answer folds into the same call's result.
func TestHandler_ElicitsThroughCaller(t *testing.T) {
	srv := httptest.NewServer(Handler([]Agent{askingAgent()}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := mcp.ConnectHTTP(ctx, srv.URL+"/agents/asker/mcp", mcp.WithElicitation(
		func(_ context.Context, q *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if !strings.Contains(q.Message, "prod or staging") {
				t.Errorf("caller saw question %q", q.Message)
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"env": "staging"}}, nil
		}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	out, err := client.CallTool(ctx, "deploy", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, "staging") {
		t.Errorf("result = %q, want the caller's answer folded in", out)
	}
}

// TestHandler_ElicitFailsClosedWithoutCallerSupport pins the fail-closed
// contract: a caller that did not advertise elicitation cannot be asked, so the
// asking call returns an error rather than hanging.
func TestHandler_ElicitFailsClosedWithoutCallerSupport(t *testing.T) {
	srv := httptest.NewServer(Handler([]Agent{askingAgent()}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := mcp.ConnectHTTP(ctx, srv.URL+"/agents/asker/mcp")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.CallTool(ctx, "deploy", nil); err == nil {
		t.Fatal("expected the call to fail closed when the caller cannot answer")
	}
}

// TestHandler_ConcurrentClientsDoNotSerialize is the point of the per-session
// instance model: two distinct clients calling the same served agent at once both
// run concurrently rather than queueing. Each call blocks on a barrier that only
// opens once both are in flight, so if the front door serialized them the barrier
// would never open and the calls would time out.
func TestHandler_ConcurrentClientsDoNotSerialize(t *testing.T) {
	const clients = 2
	var inFlight int32
	barrier := make(chan struct{})
	agent := Agent{
		Address: "worker",
		Tools:   []mcp.Tool{{Name: "work", Schema: map[string]any{"type": "object"}}},
		Resolve: func(_ context.Context, sessionID string) (Target, func(), error) {
			return Target{Call: func(ctx context.Context, _ string, _ map[string]any) (string, error) {
				if atomic.AddInt32(&inFlight, 1) == clients {
					close(barrier)
				}
				select {
				case <-barrier:
					return sessionID, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}}, func() {}, nil
		},
	}

	srv := httptest.NewServer(Handler([]Agent{agent}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := make([]string, clients)
	errs := make([]error, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client, err := mcp.ConnectHTTP(ctx, srv.URL+"/agents/worker/mcp")
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = client.Close() }()
			results[i], errs[i] = client.CallTool(ctx, "work", nil)
		}(i)
	}
	wg.Wait()

	for i := 0; i < clients; i++ {
		if errs[i] != nil {
			t.Fatalf("client %d failed (serialized?): %v", i, errs[i])
		}
	}
	// Distinct clients get distinct session ids, so they routed to distinct instances.
	if results[0] == results[1] {
		t.Errorf("both clients resolved to the same session id %q, want distinct", results[0])
	}
}

func TestHandler_SeparateEndpointPerAgent(t *testing.T) {
	mk := func(addr, tool string) Agent {
		return staticAgent(addr,
			[]mcp.Tool{{Name: tool, Schema: map[string]any{"type": "object"}}},
			func(context.Context, string, map[string]any) (string, error) { return addr, nil }, nil)
	}
	srv := httptest.NewServer(Handler([]Agent{mk("researcher", "search"), mk("web-scraper", "fetch")}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := mcp.ConnectHTTP(ctx, srv.URL+"/agents/web-scraper/mcp")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "fetch" {
		t.Fatalf("web-scraper tools = %v, want only fetch", tools)
	}
}
