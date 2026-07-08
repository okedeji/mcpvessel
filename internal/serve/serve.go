// Package serve exposes a served run's public agents over MCP-over-HTTP, one
// endpoint per agent under /agents/. It builds handlers only; the daemon owns
// the runs and decides what is public. Private tools are never registered,
// so registration is the access gate.
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

// Agent is one exposed agent: its URL segment under /agents/, its public
// tools (pre-filtered), and a per-session instance resolver.
type Agent struct {
	Address string
	Tools   []mcp.Tool

	// Resolve maps an MCP session id to that client's own instance, booting one
	// on first call so distinct clients run concurrently. release marks the call
	// no longer in flight; dispatch defers it.
	Resolve func(ctx context.Context, sessionID string) (target Target, release func(), err error)
}

// Target is one client instance a call dispatches into. BindElicit, when set,
// binds the calling client as the agent's answer channel for the call's
// duration.
type Target struct {
	Call       func(ctx context.Context, tool string, args map[string]any) (string, error)
	BindElicit func(elicit mcp.ElicitHandler) (release func())
}

// Handler builds a streamable-HTTP MCP endpoint per agent at
// /agents/<address>/mcp, each advertising only that agent's public tools.
func Handler(agents []Agent) http.Handler {
	mux := http.NewServeMux()
	for _, a := range agents {
		srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: identity.Name, Version: identity.Version}, nil)
		for _, t := range a.Tools {
			srv.AddTool(&mcpsdk.Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: inputSchema(t.Schema),
			}, dispatch(a.Resolve))
		}
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
		mux.Handle("/agents/"+a.Address+"/mcp", handler)
	}
	return mux
}

// dispatch turns one agent's resolver into an MCP tool handler. Failures come
// back as tool errors (IsError), never transport errors.
func dispatch(resolve func(ctx context.Context, sessionID string) (Target, func(), error)) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return toolError("decoding arguments: " + err.Error()), nil
		}
		target, release, err := resolve(ctx, req.Session.ID())
		if err != nil {
			return toolError(err.Error()), nil
		}
		defer release()
		if target.BindElicit != nil {
			releaseElicit := target.BindElicit(operatorElicit(req.Session))
			defer releaseElicit()
		}
		text, err := target.Call(ctx, req.Params.Name, args)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}, nil
	}
}

// operatorElicit routes the agent's mid-call questions to the calling client
// via MCP elicitation/create. A caller without the elicitation capability
// makes Elicit error, failing the asking call closed rather than hanging it.
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

// inputSchema defaults a missing schema to the empty object schema; a bare
// null is invalid JSON Schema and some clients reject it.
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
