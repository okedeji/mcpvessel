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

// connectRetryInterval paces ConnectHTTP's reconnect attempts while a freshly
// started agent is still binding its port. It is short because the failure mode
// is a fast connection-refused, not a slow timeout; the caller's context
// deadline, not this interval, bounds the total wait.
const connectRetryInterval = 100 * time.Millisecond

// Client is an open MCP session against a single agent process.
//
// Construct with Connect. Always Close in a defer to release the
// underlying session goroutines and stream readers.
type Client struct {
	session *mcpsdk.ClientSession
}

// ElicitRequest is a question an agent raises in the middle of a call: a
// human-readable message and, when it wants a structured answer, a JSON Schema
// of the fields it expects. It is the agentcage-shaped view of MCP's
// elicitation/create.
type ElicitRequest struct {
	Message string
	Schema  map[string]any
}

// ElicitResult is the caller's answer. Action is "accept", "decline", or
// "cancel"; Content holds the submitted fields and is present only on "accept".
type ElicitResult struct {
	Action  string
	Content map[string]any
}

// ElicitHandler answers an agent's mid-call question. serve supplies one backed
// by the operator's MCP client. Wiring a handler is also what advertises the
// elicitation capability, so an agent can only ask when a handler is present;
// that is the gate that keeps a one-shot run/call from offering a question
// channel that has no one on the other end.
type ElicitHandler func(ctx context.Context, q *ElicitRequest) (*ElicitResult, error)

// Option configures a Client at connect time.
type Option func(*options)

type options struct {
	onElicit ElicitHandler
}

// WithElicitation makes the client answer the server's elicitation/create
// requests through h and advertise the elicitation capability so the server may
// ask. A nil h leaves the capability unadvertised, the default, so the server
// cannot elicit.
func WithElicitation(h ElicitHandler) Option {
	return func(o *options) { o.onElicit = h }
}

// clientOptions folds the agentcage options into the SDK's ClientOptions,
// returning nil when nothing is set so the client advertises no extra
// capabilities. The SDK auto-advertises the elicitation capability the moment
// an ElicitationHandler is non-nil, so a handler is set only when the caller
// asked for one.
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

// Connect establishes an MCP session over the given stdio pair.
//
// `reader` is what we read agent responses from (the agent's stdout).
// `writer` is what we send agent requests to (the agent's stdin).
// Both are wrapped with io.NopCloser before reaching the SDK; the
// caller owns the lifecycle of the underlying streams.
//
// The MCP handshake (initialize) runs as part of Connect; by the time
// it returns, the agent has reported its protocol version and is ready
// for tool calls.
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

// ConnectHTTP establishes an MCP session against an agent serving streamable
// HTTP, the transport a detached cage uses (AGENTCAGE_SERVE_HTTP) and the way
// the daemon reaches a root it does not hold over stdio. endpoint is the full
// MCP URL, e.g. http://127.0.0.1:9123/mcp.
//
// A freshly started cage may not have bound its port yet, so the initial
// connect is retried until it succeeds or ctx is done. The retry is on the
// handshake, not on a hung server: connection-refused fails fast, so the wait
// is paced by connectRetryInterval and bounded by the caller's ctx deadline.
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

// nopWriteCloser adapts an io.Writer into an io.WriteCloser whose
// Close is a no-op, mirroring io.NopCloser for readers.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Close ends the session. Safe to call once; subsequent calls return
// the original error from the SDK.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

// Tool is the agentcage-shaped view of one tool the agent exposes: its
// name, description, and input schema as the agent's MCP server reports
// them. Build-time introspection reads these to fill the bundle's tool
// catalog. Schema is nil when the agent declares no input schema.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ListTools returns every tool the connected agent advertises. Used by
// the CLI to look up the default tool's name when the operator did not
// pass --tool explicitly. Does not paginate; agents are expected to
// expose a small handful of tools.
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

// schemaToMap normalizes an MCP tool's input schema to a map. The SDK
// hands the client a map[string]any already, but the field is typed `any`
// and a server may marshal a different concrete type, so anything else is
// round-tripped through JSON. A schema that will not marshal is dropped
// rather than failing the listing.
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

// CallTool invokes name with the given arguments and returns the text
// content of the first text block in the response. Non-text content
// (images, embedded resources) is ignored at this layer; if a use case
// needs it we add a method that returns the structured CallToolResult.
//
// If the tool returned an error (CallToolResult.IsError or any error
// embedded in the result), CallTool returns it wrapped with the name.
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

// firstText returns the text of the first TextContent block in blocks,
// or empty string if none is present.
func firstText(blocks []mcpsdk.Content) string {
	for _, c := range blocks {
		if t, ok := c.(*mcpsdk.TextContent); ok {
			return t.Text
		}
	}
	return ""
}
