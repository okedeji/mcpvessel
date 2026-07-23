// Package serve exposes a served run's public agents over MCP-over-HTTP and
// plain JSON-over-HTTP on one front door. It builds handlers only; the daemon
// owns the runs and decides what is public. Private tools are never registered,
// so registration is the access gate.
package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/identity"
	"github.com/okedeji/mcpvessel/internal/mcp"
)

// Agent is one exposed agent: its URL segment under /agents/, its public
// tools (pre-filtered), its main tool, and a per-session instance resolver.
type Agent struct {
	Address string
	Tools   []mcp.Tool

	// Main is the agent's main tool, the target of the POST /agents/<address>
	// prompt shortcut. Empty for a tool collection with no MAIN.
	Main string

	// Resolve maps an MCP session id to that client's own instance, booting one
	// on first call so distinct clients run concurrently. release marks the call
	// no longer in flight; dispatch defers it.
	Resolve func(ctx context.Context, sessionID string) (target Target, release func(), err error)
}

// Target is one client instance a call dispatches into. BindElicit, when set,
// binds the calling client as the agent's answer channel for the call's
// duration. CallStream is Call with a progress sink, used by the REST SSE
// path; when nil the SSE path falls back to Call and delivers one final event.
type Target struct {
	Call       func(ctx context.Context, tool string, args map[string]any) (string, error)
	CallStream func(ctx context.Context, tool string, args map[string]any, onProgress mcp.ProgressHandler) (string, error)
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

// Handler builds the front door: each exposed agent is reachable over
// MCP-over-HTTP and over plain JSON-over-HTTP, and the merged endpoints carry
// every agent's tools at once. The plain path reuses the same dispatch as MCP,
// so it is a second door, not a bypass; unlike MCP it carries no session, so
// each plain-HTTP call runs in its own ephemeral instance (see
// ephemeralSessionID) and never shares state with another caller.
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

		mux.HandleFunc("POST /agents/"+a.Address+"/tools/{tool}", func(w http.ResponseWriter, r *http.Request) {
			httpCallTool(w, r, a, r.PathValue("tool"))
		})
		if a.Main != "" {
			mux.HandleFunc("POST /agents/"+a.Address, func(w http.ResponseWriter, r *http.Request) {
				httpPrompt(w, r, a)
			})
		}
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

		byName := make(map[string]FlatTool, len(flat))
		for _, ft := range flat {
			byName[ft.Name] = ft
		}
		mux.HandleFunc("POST /tools/{name}", func(w http.ResponseWriter, r *http.Request) {
			ft, ok := byName[r.PathValue("name")]
			if !ok {
				writeJSONError(w, http.StatusNotFound, fmt.Sprintf("unknown tool %q", r.PathValue("name")))
				return
			}
			httpCallTool(w, r, agents[ft.Agent], ft.Tool.Name)
		})
	}
	return mux
}

// defaultMaxRequestBytes caps a single plain-HTTP request body (tool arguments
// or a prompt). The front door is unauthenticated, so this is a DoS floor: a
// caller cannot make the daemon buffer an unbounded body before the JSON
// decode. VESSEL_MAX_SERVE_BODY raises it for legitimately large payloads.
const defaultMaxRequestBytes = 1 << 20 // 1 MiB

// maxRequestBytes resolves the body cap once per process: the operator's
// VESSEL_MAX_SERVE_BODY if set, else the default. Resolved lazily so tests and
// config changes are picked up on first use, not at package init.
var maxRequestBytes = sync.OnceValue(func() int64 {
	return config.ByteSizeEnv(env.MaxServeBody, defaultMaxRequestBytes)
})

// ephemeralSessionPrefix marks a session id minted per plain-HTTP request. The
// daemon recognizes it (see IsEphemeralSession) and drops the instance the
// moment the call returns instead of pinning a client slot until the idle TTL.
const ephemeralSessionPrefix = "rest-ephemeral-"

// IsEphemeralSession reports whether id was minted by ephemeralSessionID for a
// single plain-HTTP request, so the resolver can reclaim its instance on
// release rather than hold it for reuse the way it holds an MCP session.
func IsEphemeralSession(id string) bool {
	return strings.HasPrefix(id, ephemeralSessionPrefix)
}

// ephemeralSessionID mints a fresh, unguessable session key for one plain-HTTP
// request. Every REST call thus resolves to its own instance and can never read
// another caller's conversation, files, or elicitation channel: unlike MCP,
// plain HTTP carries no session, so a shared key would land every caller in one
// live instance. A rand failure (near impossible) falls back to a nanosecond
// stamp, still per-request unique.
func ephemeralSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%d", ephemeralSessionPrefix, time.Now().UnixNano())
	}
	return ephemeralSessionPrefix + hex.EncodeToString(b[:])
}

// statusForBodyError maps a request-body read failure to its HTTP status: 413
// when the body ran past maxRequestBytes, 400 for anything else (malformed
// JSON, a short read). The MaxBytesReader error survives fmt.Errorf %w
// wrapping, so errors.As still finds it.
func statusForBodyError(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// httpCallTool invokes a public tool with the JSON body as its arguments; a
// tool outside the agent's public set is a 404.
func httpCallTool(w http.ResponseWriter, r *http.Request, a Agent, tool string) {
	if !publicTool(a, tool) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("unknown tool %q", tool))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes())
	args, err := readArgs(r)
	if err != nil {
		writeJSONError(w, statusForBodyError(err), err.Error())
		return
	}
	// The tool body is the tool's own arguments, so streaming is asked for out
	// of band (Accept header or ?stream=), never a body field that would land
	// in the arguments.
	if wantsStream(r) {
		httpInvokeStream(w, r, a, tool, args)
		return
	}
	httpInvoke(w, r, a, tool, args)
}

// httpPrompt routes {"prompt": "..."} (or a raw messages array) to the agent's
// main tool, wrapping a prompt as one user message the way run does.
func httpPrompt(w http.ResponseWriter, r *http.Request, a Agent) {
	var body struct {
		Prompt   string           `json:"prompt"`
		Messages []map[string]any `json:"messages"`
		Stream   bool             `json:"stream"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes())
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeJSONError(w, statusForBodyError(err), "decoding request: "+err.Error())
		return
	}
	var args map[string]any
	switch {
	case len(body.Messages) > 0:
		args = map[string]any{"messages": body.Messages}
	case body.Prompt != "":
		args = map[string]any{"messages": []map[string]any{{"role": "user", "content": body.Prompt}}}
	default:
		writeJSONError(w, http.StatusBadRequest, "provide a prompt or messages in the body")
		return
	}
	// The prompt envelope is ours, so "stream": true in the body is a clean
	// opt-in alongside the Accept header and query param.
	if body.Stream || wantsStream(r) {
		httpInvokeStream(w, r, a, a.Main, args)
		return
	}
	httpInvoke(w, r, a, a.Main, args)
}

// wantsStream reports an out-of-band request for SSE: the streaming Accept
// header (what an EventSource sends) or ?stream=true|1.
func wantsStream(r *http.Request) bool {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return true
	}
	switch strings.ToLower(r.URL.Query().Get("stream")) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// sseHeartbeat is how often an idle stream sends an SSE comment, so a proxy or
// client does not time out during a long tool-call phase that emits no deltas.
const sseHeartbeat = 15 * time.Second

// sseWriteTimeout bounds a single SSE write+flush. It is a per-write deadline,
// reset before every event, not a cap on the stream's total length: a client
// reading normally clears each event in milliseconds, so a legitimate hours-long
// stream is unaffected, while a slow or stalled reader that stops draining the
// socket trips the deadline and frees the serving slot instead of pinning it
// open. It is comfortably longer than sseHeartbeat so a healthy idle stream
// never races its own keepalive.
const sseWriteTimeout = 45 * time.Second

// httpInvokeStream answers over Server-Sent Events: `delta` events carry answer
// chunks as the agent generates them, a final `done` event carries the whole
// result, and `error` carries a failure. An agent that reports no progress (or
// a target with no streaming path) still works, delivering one `done`. The
// event vocabulary is documented in the reasoner contract, so any agent that
// emits MCP progress notifications streams here for free.
func httpInvokeStream(w http.ResponseWriter, r *http.Request, a Agent, tool string, args map[string]any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flushing means no live stream; fall back to one JSON object rather
		// than buffer an SSE body the caller only sees at the end.
		httpInvoke(w, r, a, tool, args)
		return
	}
	// A failed write (slow-reader deadline, dropped connection) cancels this
	// context so the streaming call unwinds and the handler returns instead of
	// generating for a client that stopped reading.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	target, release, err := a.Resolve(ctx, ephemeralSessionID())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer release()

	// SetWriteDeadline caps how long any single write may block on the socket,
	// so a slow reader trips the deadline instead of pinning the handler. Flush
	// goes through the controller too; both degrade to no-ops if the writer does
	// not support them (the flusher check above already covers the common path).
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// One writer at a time: progress arrives on the session's read goroutine,
	// the heartbeat on a ticker goroutine, and the terminal event on this one.
	// The first write that fails cancels the call; later writes short-circuit.
	var mu sync.Mutex
	failed := false
	write := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		if failed {
			return
		}
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		if _, err := io.WriteString(w, s); err != nil {
			failed = true
			cancel()
			return
		}
		if err := rc.Flush(); err != nil {
			failed = true
			cancel()
		}
	}
	writeEvent := func(event string, data any) {
		b, _ := json.Marshal(data)
		write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
	}

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(sseHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				write(": keepalive\n\n")
			}
		}
	}()
	defer close(stop)

	// A target with no streaming path (or an agent that emits no progress)
	// still yields a valid stream: one final `done` with the whole answer.
	if target.CallStream == nil {
		text, err := target.Call(ctx, tool, args)
		if err != nil {
			writeEvent("error", map[string]any{"error": err.Error()})
			return
		}
		writeEvent("done", map[string]any{"result": resultValue(text)})
		return
	}

	onProgress := func(chunk mcp.ProgressChunk) {
		if chunk.Message == "" {
			return
		}
		writeEvent("delta", map[string]any{"text": chunk.Message})
	}
	text, err := target.CallStream(ctx, tool, args, onProgress)
	if err != nil {
		writeEvent("error", map[string]any{"error": err.Error()})
		return
	}
	writeEvent("done", map[string]any{"result": resultValue(text)})
}

// httpInvoke resolves this request's own ephemeral instance and calls tool. It
// binds no elicitation channel: a curl caller cannot answer a mid-call
// question, so an agent that asks fails closed.
func httpInvoke(w http.ResponseWriter, r *http.Request, a Agent, tool string, args map[string]any) {
	target, release, err := a.Resolve(r.Context(), ephemeralSessionID())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer release()
	text, err := target.Call(r.Context(), tool, args)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": resultValue(text)})
}

// resultValue upgrades a tool's text output to real JSON when it is a JSON
// object or array, so an HTTP caller gets {"result": {...}} instead of the
// same JSON escaped inside a string, parsed twice. Scalars stay strings: a
// tool that answers the text "42" or "true" means the text, not the value.
func resultValue(text string) any {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return text
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return text
	}
	return v
}

func publicTool(a Agent, tool string) bool {
	for _, t := range a.Tools {
		if t.Name == tool {
			return true
		}
	}
	return false
}

// readArgs decodes the JSON body as the tool's arguments; an empty body is none.
func readArgs(r *http.Request) (map[string]any, error) {
	args := map[string]any{}
	if r.Body == nil {
		return args, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		if err == io.EOF {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("decoding arguments: %w", err)
	}
	return args, nil
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// dispatch turns one agent's resolver into an MCP tool handler, forwarding
// under the given tool name; empty means the request's own name (the
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
