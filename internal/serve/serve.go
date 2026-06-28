// Package serve is the external MCP front door: it exposes a served run's
// public agents to outside callers over MCP-over-HTTP, one endpoint per agent.
//
// It is a handler builder, not a server or a run owner. The daemon boots and
// holds the runs, decides which agents and tools are public, and hands serve a
// dispatch closure per agent; serve turns each into an MCP endpoint under
// /agents/. A private tool is never registered on its agent's endpoint, so it
// cannot be called: registration is the access gate, the same encapsulation the
// MCP gateway enforces inside a run, applied at the host edge.
package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/okedeji/agentcage/internal/identity"
	"github.com/okedeji/agentcage/internal/mcp"
)

// Agent is one exposed agent the front door serves: the URL segment it answers
// on under /agents/, the public tools an external caller may invoke (already
// filtered to public, with their schemas), and the dispatch into the agent's
// held run.
type Agent struct {
	Address string
	Tools   []mcp.Tool
	Call    func(ctx context.Context, tool string, args map[string]any) (string, error)

	// BindElicit, when set, binds the calling client as the agent's answer
	// channel for the duration of a call, so the agent can ask the caller a
	// question mid-call and resume with the answer. It returns a release the
	// dispatch defers. Nil leaves the agent unable to elicit (the front door
	// supplies it only for interactive runs; tests leave it unset).
	BindElicit func(elicit mcp.ElicitHandler) (release func())
}

// Handler builds the front door: a streamable-HTTP MCP endpoint per agent at
// /agents/<address>/mcp, each advertising only that agent's public tools and
// dispatching a call into its held run.
func Handler(agents []Agent) http.Handler {
	mux := http.NewServeMux()
	for _, a := range agents {
		srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: identity.Name, Version: identity.Version}, nil)
		for _, t := range a.Tools {
			srv.AddTool(&mcpsdk.Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: inputSchema(t.Schema),
			}, dispatch(a.Call, a.BindElicit))
		}
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
		mux.Handle("/agents/"+a.Address+"/mcp", handler)
	}
	return mux
}

// dispatch turns one agent's Call closure into an MCP tool handler. A failed
// call comes back as a tool error (IsError) carrying the message, not a
// transport failure, so the caller's MCP client surfaces it like any tool error.
//
// When bind is set, the calling client becomes the agent's answer channel for
// this call, so a question the agent raises mid-call rides MCP's elicitation
// back to whoever made the call. The bind is released when the call returns.
func dispatch(call func(context.Context, string, map[string]any) (string, error), bind func(mcp.ElicitHandler) func()) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return toolError("decoding arguments: " + err.Error()), nil
		}
		if bind != nil {
			release := bind(operatorElicit(req.Session))
			defer release()
		}
		text, err := call(ctx, req.Params.Name, args)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}, nil
	}
}

// operatorElicit turns the calling client's session into an answer channel for
// the agent's mid-call questions. req.Session is the external caller; Elicit
// rides MCP's elicitation/create back to it, so the agent asks whoever made the
// call. A caller that did not advertise the elicitation capability makes Elicit
// error, which fails the asking call closed rather than hanging it.
func operatorElicit(session *mcpsdk.ServerSession) mcp.ElicitHandler {
	return func(ctx context.Context, q *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		res, err := session.Elicit(ctx, &mcpsdk.ElicitParams{
			Message:         q.Message,
			RequestedSchema: q.Schema,
		})
		if err != nil {
			return nil, fmt.Errorf("asking the caller: %w", err)
		}
		return &mcp.ElicitResult{Action: res.Action, Content: res.Content}, nil
	}
}

func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return args, nil
}

// inputSchema defaults a tool with no declared schema to the empty object
// schema, so the listing is valid JSON Schema rather than a bare null an
// external client may reject.
func inputSchema(schema map[string]any) any {
	if schema == nil {
		return map[string]any{"type": "object"}
	}
	return schema
}

func toolError(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
	}
}
