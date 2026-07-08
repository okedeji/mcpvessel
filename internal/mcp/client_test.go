package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testServer starts an in-memory MCP server on one end of a net.Pipe and a
// connected Client on the other. addTools configures the server; nil means
// no tools. Cleanup is registered via t.Cleanup.
func testServer(t *testing.T, addTools func(s *mcpsdk.Server)) (*Client, *mcpsdk.Server) {
	t.Helper()

	srvConn, cliConn := net.Pipe()
	t.Cleanup(func() {
		_ = srvConn.Close()
		_ = cliConn.Close()
	})

	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "agentcage-test-server",
		Version: "0.0.0",
	}, nil)
	if addTools != nil {
		addTools(server)
	}

	serverCtx, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)

	go func() {
		_ = server.Run(serverCtx, &mcpsdk.IOTransport{
			Reader: srvConn,
			Writer: srvConn,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Connect(ctx, cliConn, cliConn)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, server
}

func TestConnect_HandshakeSucceeds(t *testing.T) {
	_, _ = testServer(t, nil)
	// Reaching here means the MCP initialize round-trip completed.
}

func TestConnectHTTP_CallsToolOverHTTP(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "agentcage-test-server",
		Version: "0.0.0",
	}, nil)
	registerEchoTool(server)

	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := ConnectHTTP(ctx, ts.URL+"/mcp")
	if err != nil {
		t.Fatalf("ConnectHTTP: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := client.CallTool(ctx, "echo", map[string]any{"message": "over-http"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "over-http" {
		t.Errorf("CallTool echo = %q, want %q", got, "over-http")
	}
}

// TestConnectHTTP_RetriesThenDeadline pins the cold-start contract:
// ConnectHTTP keeps retrying rather than failing on the first refusal, and
// errors out when the deadline passes rather than hanging.
func TestConnectHTTP_RetriesThenDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := ConnectHTTP(ctx, "http://127.0.0.1:1/mcp"); err == nil {
		t.Fatal("expected an error connecting to a closed port")
	}
}

func TestConnect_AnswersElicitation(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	t.Cleanup(func() {
		_ = srvConn.Close()
		_ = cliConn.Close()
	})

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "agentcage-test-server", Version: "0.0.0"}, nil)
	registerAskTool(server)

	serverCtx, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() {
		_ = server.Run(serverCtx, &mcpsdk.IOTransport{Reader: srvConn, Writer: srvConn})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var asked string
	client, err := Connect(ctx, cliConn, cliConn, WithElicitation(func(_ context.Context, q *ElicitRequest) (*ElicitResult, error) {
		asked = q.Message
		return &ElicitResult{Action: "accept", Content: map[string]any{"answer": "blue"}}, nil
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := client.CallTool(ctx, "ask", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if asked != "color?" {
		t.Errorf("handler saw %q, want color?", asked)
	}
	if !contains(got, "blue") {
		t.Errorf("result = %q, want it to carry the answer", got)
	}
}

// A plain client never advertises elicitation, so a server that tries to
// ask fails the call closed.
func TestConnect_ElicitationUnadvertisedByDefault(t *testing.T) {
	client, _ := testServer(t, registerAskTool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.CallTool(ctx, "ask", nil); err == nil {
		t.Fatal("expected the call to fail closed when the client cannot answer an elicitation")
	}
}

func TestListTools_EmptyServer(t *testing.T) {
	client, _ := testServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("ListTools = %d tools, want 0 (got %+v)", len(tools), tools)
	}
}

func TestListTools_ReturnsRegisteredTools(t *testing.T) {
	client, _ := testServer(t, func(s *mcpsdk.Server) {
		registerEchoTool(s)
		registerGreetTool(s)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]string{}
	for _, tool := range tools {
		names[tool.Name] = tool.Description
	}
	if names["echo"] != "echoes its input back" {
		t.Errorf("missing echo or wrong description: %+v", names)
	}
	if names["greet"] != "greets by name" {
		t.Errorf("missing greet or wrong description: %+v", names)
	}
}

func TestListTools_CapturesInputSchema(t *testing.T) {
	client, _ := testServer(t, registerEchoTool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var echo *Tool
	for i := range tools {
		if tools[i].Name == "echo" {
			echo = &tools[i]
		}
	}
	if echo == nil {
		t.Fatal("echo tool not listed")
	}
	if echo.Schema == nil {
		t.Fatal("echo tool's input schema was not captured")
	}
	// AddTool infers the schema from echoInput{Message string}.
	props, ok := echo.Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties object: %+v", echo.Schema)
	}
	if _, ok := props["message"]; !ok {
		t.Errorf("schema properties missing 'message': %+v", props)
	}
}

func TestSchemaToMap(t *testing.T) {
	if schemaToMap(nil) != nil {
		t.Error("nil schema should map to nil")
	}
	if got := schemaToMap(map[string]any{"type": "object"}); got["type"] != "object" {
		t.Errorf("map passthrough failed: %+v", got)
	}
	// Anything else round-trips through JSON.
	type schema struct {
		Type string `json:"type"`
	}
	if got := schemaToMap(schema{Type: "object"}); got == nil || got["type"] != "object" {
		t.Errorf("struct should round-trip to map, got %+v", got)
	}
}

func TestCallTool_ReturnsTextContent(t *testing.T) {
	client, _ := testServer(t, registerEchoTool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := client.CallTool(ctx, "echo", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "hello" {
		t.Errorf("CallTool echo = %q, want %q", got, "hello")
	}
}

func TestCallTool_UnknownTool(t *testing.T) {
	client, _ := testServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.CallTool(ctx, "nonexistent", nil)
	if err == nil {
		t.Fatalf("expected error for unknown tool, got nil")
	}
	if !errorContains(err, "nonexistent") {
		t.Errorf("error does not name the tool: %v", err)
	}
}

func TestCallTool_ToolReturnsIsErrorResult(t *testing.T) {
	client, _ := testServer(t, registerFailingTool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.CallTool(ctx, "fail", nil)
	if err == nil {
		t.Fatalf("expected error from failing tool, got nil")
	}
	if !errorContains(err, "fail") {
		t.Errorf("error does not name the tool: %v", err)
	}
}

func TestClose_IsSafeOnNil(t *testing.T) {
	var c *Client
	if err := c.Close(); err != nil {
		t.Errorf("Close on nil receiver returned: %v", err)
	}
	c = &Client{}
	if err := c.Close(); err != nil {
		t.Errorf("Close on zero struct returned: %v", err)
	}
}

func TestFirstText_PicksFirstTextBlock(t *testing.T) {
	blocks := []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "first"},
		&mcpsdk.TextContent{Text: "second"},
	}
	if got := firstText(blocks); got != "first" {
		t.Errorf("firstText = %q, want %q", got, "first")
	}
}

func TestFirstText_EmptyWhenNoText(t *testing.T) {
	if got := firstText(nil); got != "" {
		t.Errorf("firstText(nil) = %q, want empty", got)
	}
	if got := firstText([]mcpsdk.Content{}); got != "" {
		t.Errorf("firstText(empty slice) = %q, want empty", got)
	}
}

// ----- test fixtures: tools the in-memory server exposes -----

type echoInput struct {
	Message string `json:"message"`
}
type echoOutput struct {
	Echoed string `json:"echoed"`
}

func registerEchoTool(s *mcpsdk.Server) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "echo",
		Description: "echoes its input back",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoInput) (*mcpsdk.CallToolResult, echoOutput, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Message}},
		}, echoOutput{Echoed: in.Message}, nil
	})
}

type greetInput struct {
	Name string `json:"name"`
}
type greetOutput struct {
	Greeting string `json:"greeting"`
}

func registerGreetTool(s *mcpsdk.Server) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "greet",
		Description: "greets by name",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in greetInput) (*mcpsdk.CallToolResult, greetOutput, error) {
		text := "hello " + in.Name
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		}, greetOutput{Greeting: text}, nil
	})
}

// registerAskTool is the server side of an elicitation round trip: it asks
// the caller a question mid-call and folds the answer into its result.
func registerAskTool(s *mcpsdk.Server) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "ask",
		Description: "asks the caller a question, then answers",
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
		res, err := req.Session.Elicit(ctx, &mcpsdk.ElicitParams{Message: "color?"})
		if err != nil {
			return nil, struct{}{}, err
		}
		answer, _ := res.Content["answer"].(string)
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "you said " + answer}},
		}, struct{}{}, nil
	})
}

func registerFailingTool(s *mcpsdk.Server) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "fail",
		Description: "always fails",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
		return nil, struct{}{}, errors.New("intentional failure")
	})
}

func errorContains(err error, needle string) bool {
	return err != nil && contains(err.Error(), needle)
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
