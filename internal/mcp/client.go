package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/okedeji/agentcage/internal/identity"
)

// connectRetryInterval paces ConnectHTTP's reconnects while a fresh agent
// is still binding its port. Short because the failure mode is a fast
// connection-refused; the caller's ctx deadline bounds the total wait.
const connectRetryInterval = 100 * time.Millisecond

// Client is an open MCP session against a single agent process. Close it
// to release the session goroutines and stream readers.
type Client struct {
	session *mcpsdk.ClientSession
}

// ElicitRequest is a question an agent raises mid-call: a message and,
// when it wants a structured answer, a JSON Schema of the fields.
type ElicitRequest struct {
	Message string
	Schema  map[string]any
}

// ElicitResult is the caller's answer. Action is "accept", "decline", or
// "cancel"; Content is present only on "accept".
type ElicitResult struct {
	Action  string
	Content map[string]any
}

// ElicitHandler answers an agent's mid-call question. Wiring a handler is
// what advertises the elicitation capability: no handler, no question
// channel with no one on the other end.
type ElicitHandler func(ctx context.Context, q *ElicitRequest) (*ElicitResult, error)

// Option configures a Client at connect time.
type Option func(*options)

type options struct {
	onElicit ElicitHandler
}

// WithElicitation routes the server's elicitation/create requests through h
// and advertises the capability. A nil h leaves it unadvertised, so the
// server cannot elicit.
func WithElicitation(h ElicitHandler) Option {
	return func(o *options) { o.onElicit = h }
}

// clientOptions returns nil when nothing is set: the SDK auto-advertises
// elicitation the moment an ElicitationHandler is non-nil, so one is wired
// only when the caller asked.
func clientOptions(opts []Option) *mcpsdk.ClientOptions {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.onElicit == nil {
		return nil
	}
	h := o.onElicit
	return &mcpsdk.ClientOptions{
		ElicitationHandler: func(ctx context.Context, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
			res, err := h(ctx, &ElicitRequest{
				Message: req.Params.Message,
				Schema:  schemaToMap(req.Params.RequestedSchema),
			})
			if err != nil {
				return nil, err
			}
			return &mcpsdk.ElicitResult{Action: res.Action, Content: res.Content}, nil
		},
	}
}

// Connect establishes an MCP session over a stdio pair: reader is the
// agent's stdout, writer its stdin. The caller owns both streams. The MCP
// initialize handshake completes before Connect returns.
func Connect(ctx context.Context, reader io.Reader, writer io.Writer, opts ...Option) (*Client, error) {
	c := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    identity.Name,
		Version: identity.Version,
	}, clientOptions(opts))
	session, err := c.Connect(ctx, &mcpsdk.IOTransport{
		Reader: io.NopCloser(reader),
		Writer: nopWriteCloser{writer},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect: %w", err)
	}
	return &Client{session: session}, nil
}

// ConnectHTTP establishes an MCP session against an agent serving
// streamable HTTP, the transport a detached cage uses. endpoint is the full
// MCP URL, e.g. http://127.0.0.1:9123/mcp. A fresh cage may not have bound
// its port yet, so the handshake is retried until it succeeds or ctx is
// done; connection-refused fails fast, so ctx bounds the wait.
func ConnectHTTP(ctx context.Context, endpoint string, opts ...Option) (*Client, error) {
	c := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    identity.Name,
		Version: identity.Version,
	}, clientOptions(opts))
	for {
		session, err := c.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: endpoint}, nil)
		if err == nil {
			return &Client{session: session}, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("mcp http connect to %s: %w", endpoint, err)
		case <-time.After(connectRetryInterval):
		}
	}
}

// nopWriteCloser mirrors io.NopCloser for writers.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Close ends the session. Nil-safe.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

// Tool is the agentcage-shaped view of one tool the agent exposes: its
// name, description, and input schema as the agent's MCP server reports
// them. Schema is nil when the agent declares no input schema.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ListTools returns every tool the connected agent advertises. It does not
// paginate; agents are expected to expose a small handful.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	res, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp tools/list: %w", err)
	}
	out := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		out = append(out, Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      schemaToMap(t.InputSchema),
		})
	}
	return out, nil
}

// schemaToMap normalizes a tool's input schema. The SDK usually hands over
// a map[string]any, but the field is typed any, so other concrete types
// round-trip through JSON. A schema that will not marshal is dropped rather
// than failing the listing.
func schemaToMap(schema any) map[string]any {
	if schema == nil {
		return nil
	}
	if m, ok := schema.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// CallTool invokes name and returns the first text block of the response.
// Non-text content (images, embedded resources) is ignored at this layer.
// A tool-reported error (IsError) comes back wrapped with the tool name.
func (c *Client) CallTool(ctx context.Context, name string, args any) (string, error) {
	res, err := c.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("mcp tools/call %s: %w", name, err)
	}
	if res.IsError {
		return "", fmt.Errorf("mcp tools/call %s: tool returned an error: %s", name, firstText(res.Content))
	}
	return firstText(res.Content), nil
}

func firstText(blocks []mcpsdk.Content) string {
	for _, c := range blocks {
		if t, ok := c.(*mcpsdk.TextContent); ok {
			return t.Text
		}
	}
	return ""
}
