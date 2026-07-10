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
	"strings"

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

// FlatPath is where the merged endpoint is mounted: one MCP server
// advertising every exposed agent's public tools, so an MCP client (Cursor,
// Claude) configures a single URL no matter how many bundles are served.
const FlatPath = "/mcp"

// FlatTool is one entry on the merged endpoint: the name it is advertised
// under (always <agent>_<tool>) and the index of the exposed agent that
// serves it.
type FlatTool struct {
	Name  string
	Tool  mcp.Tool
	Agent int
}

// FlatTools merges every agent's public tools into the flat endpoint's single
// namespace as <agent>_<tool>. Every name is prefixed, never just colliding
// ones, so adding a bundle to a serve can never rename an existing tool out
// from under a configured client. A collision that survives prefixing (two
// addresses sanitizing identically) is an error for the operator, not a
// silent drop.
func FlatTools(agents []Agent) ([]FlatTool, error) {
	seen := map[string]string{}
	var out []FlatTool
	for i, a := range agents {
		for _, t := range a.Tools {
			name := toolPrefix(a.Address) + "_" + t.Name
			owner := a.Address + "/" + t.Name
			if other, dup := seen[name]; dup {
				return nil, fmt.Errorf("tools %s and %s both flatten to %q on %s; hide one agent with --no-expose", other, owner, name, FlatPath)
			}
			seen[name] = owner
			out = append(out, FlatTool{Name: name, Tool: t, Agent: i})
		}
	}
	return out, nil
}

// toolPrefix reduces an agent address to MCP-tool-name-safe characters.
func toolPrefix(addr string) string {
	var b strings.Builder
	for _, r := range addr {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// Handler builds the front door: a streamable-HTTP MCP endpoint per agent at
// /agents/<address>/mcp, each advertising only that agent's public tools,
// plus the merged endpoint at FlatPath advertising flat.
func Handler(agents []Agent, flat []FlatTool) http.Handler {
	mux := http.NewServeMux()
	for i := range agents {
		a := agents[i]
		srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: identity.Name, Version: identity.Version}, nil)
		for _, t := range a.Tools {
			srv.AddTool(&mcpsdk.Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: inputSchema(t.Schema),
			}, dispatch("", a.Resolve))
		}
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
		mux.Handle("/agents/"+a.Address+"/mcp", handler)
	}
	if len(flat) > 0 {
		srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: identity.Name, Version: identity.Version}, nil)
		for _, ft := range flat {
			a := agents[ft.Agent]
			srv.AddTool(&mcpsdk.Tool{
				Name:        ft.Name,
				Description: ft.Tool.Description,
				InputSchema: inputSchema(ft.Tool.Schema),
			}, dispatch(ft.Tool.Name, a.Resolve))
		}
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
		mux.Handle(FlatPath, handler)
		mux.Handle(FlatPath+"/", handler)
	}
	return mux
}

// dispatch turns one agent's resolver into an MCP tool handler, forwarding
// under the given tool name — empty means the request's own name (the
// per-agent endpoints, where names are never prefixed). Failures come back as
// tool errors (IsError), never transport errors.
func dispatch(tool string, resolve func(ctx context.Context, sessionID string) (Target, func(), error)) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		name := tool
		if name == "" {
			name = req.Params.Name
		}
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
		text, err := target.Call(ctx, name, args)
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
