package mcpgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The gateway's narrow MCP-aware filters, the only points where it parses
// rather than streams. Each fails open toward forwarding the original bytes;
// an unrecognized parse is never silently dropped.

const sessionHeader = "Mcp-Session-Id"

// backChannelTimeout bounds the POST that hands a sub-agent the operator's
// answer. Short: the long human wait already happened inside Elicit; this is
// one hop on the per-run network.
const backChannelTimeout = 10 * time.Second

// rewriteInitialize adds the elicitation capability to an initialize request,
// whatever the parent's own client declared, so the sub-agent is willing to
// ask questions the gateway routes to the operator. Any other body is
// returned untouched.
func rewriteInitialize(body []byte) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(body, &msg) != nil {
		return body
	}
	var method string
	if json.Unmarshal(msg["method"], &method) != nil || method != "initialize" {
		return body
	}
	var params map[string]json.RawMessage
	if len(msg["params"]) == 0 || json.Unmarshal(msg["params"], &params) != nil {
		return body
	}
	var caps map[string]json.RawMessage
	if len(params["capabilities"]) > 0 {
		_ = json.Unmarshal(params["capabilities"], &caps)
	}
	if caps == nil {
		caps = map[string]json.RawMessage{}
	}
	if _, ok := caps["elicitation"]; ok {
		return body
	}
	caps["elicitation"] = json.RawMessage(`{}`)
	if !replaceField(params, "capabilities", caps) || !replaceField(msg, "params", params) {
		return body
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	return out
}

// replaceField re-marshals value into m[key], leaving m untouched on failure.
func replaceField(m map[string]json.RawMessage, key string, value any) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	m[key] = raw
	return true
}

// modifyResponse runs the response-side filters for an edge: strip denied
// tools from a tools/list result, intercept a sub-agent's elicitation. A plain
// application/json response carries only a result, so it needs the deny strip
// alone; an event stream can also carry the question, so it goes through the
// streaming filter.
func (g *Gateway) modifyResponse(edge string) func(*http.Response) error {
	return func(resp *http.Response) error {
		// deny is read here, not captured above: New builds this hook before
		// filling the deny map. The map is write-once and complete before any
		// response arrives.
		deny := g.deny[edge]
		ct := baseMediaType(resp.Header.Get("Content-Type"))
		switch ct {
		case "text/event-stream":
			ctx := context.Background()
			sessionID, subURL := "", ""
			if resp.Request != nil {
				ctx = resp.Request.Context()
				sessionID = resp.Request.Header.Get(sessionHeader)
				if resp.Request.URL != nil {
					subURL = resp.Request.URL.String()
				}
			}
			src := resp.Body
			pr, pw := io.Pipe()
			go func() {
				err := g.filterStream(ctx, edge, sessionID, subURL, deny, src, pw)
				_ = src.Close()
				_ = pw.CloseWithError(err)
			}()
			resp.Body = pr
		case "application/json":
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return err
			}
			if stripped, changed := stripDeniedTools(body, deny); changed {
				body = stripped
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		return nil
	}
}

// filterStream copies the sub-agent's SSE response one event at a time. A
// tools/list result has its denied tools stripped; an elicitation/create is
// pulled out of the parent-bound stream, answered over the control stream,
// and the answer posted back so the sub-agent resumes. Every other event
// forwards verbatim.
func (g *Gateway) filterStream(ctx context.Context, edge, sessionID, subURL string, deny map[string]bool, src io.Reader, dst io.Writer) error {
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var event []string
	flush := func() error {
		if len(event) == 0 {
			return nil
		}
		defer func() { event = event[:0] }()
		data := sseData(event)
		if len(data) > 0 {
			if isElicitationCreate(data) {
				g.answerSubElicit(ctx, edge, sessionID, subURL, data)
				return nil
			}
			if stripped, changed := stripDeniedTools(data, deny); changed {
				return writeEvent(dst, event, stripped)
			}
		}
		return writeEvent(dst, event, nil)
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		event = append(event, line)
	}
	if err := flush(); err != nil {
		return err
	}
	return sc.Err()
}

// answerSubElicit routes a sub-agent's question to the operator and posts the
// answer back. On any failure it posts a JSON-RPC error instead, failing the
// sub-agent's call closed rather than leaving it blocked forever.
func (g *Gateway) answerSubElicit(ctx context.Context, edge, sessionID, subURL string, data []byte) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Params struct {
			Message         string         `json:"message"`
			RequestedSchema map[string]any `json:"requestedSchema"`
		} `json:"params"`
	}
	if json.Unmarshal(data, &req) != nil {
		return
	}
	ans, ok := g.Elicit(ctx, edge, ElicitQuestion{Message: req.Params.Message, Schema: req.Params.RequestedSchema})
	g.postBackChannel(ctx, subURL, sessionID, elicitResponse(req.ID, ans, ok))
}

// elicitResponse builds the JSON-RPC reply to an elicitation/create: the
// operator's answer, or an error when the operator was unreachable.
func elicitResponse(id json.RawMessage, ans ElicitAnswer, ok bool) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	type rpcError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  *ElicitAnswer   `json:"result,omitempty"`
		Error   *rpcError       `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id}
	if ok {
		a := ans
		msg.Result = &a
	} else {
		msg.Error = &rpcError{Code: -32001, Message: "the operator could not be reached to answer"}
	}
	out, _ := json.Marshal(msg)
	return out
}

// postBackChannel delivers a message to the sub-agent's MCP endpoint on the
// session the parent's call opened; the streamable transport correlates it to
// the waiting request by id.
func (g *Gateway) postBackChannel(ctx context.Context, subURL, sessionID string, body []byte) {
	if subURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, backChannelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, subURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(sessionHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// stripDeniedTools removes denied tools from a tools/list result so a
// parent's LLM never discovers a tool the gateway would reject the call to.
// A message with no result.tools array is left untouched.
func stripDeniedTools(data []byte, deny map[string]bool) ([]byte, bool) {
	if len(deny) == 0 {
		return data, false
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil {
		return data, false
	}
	var result map[string]json.RawMessage
	if len(msg["result"]) == 0 || json.Unmarshal(msg["result"], &result) != nil {
		return data, false
	}
	var tools []json.RawMessage
	if len(result["tools"]) == 0 || json.Unmarshal(result["tools"], &tools) != nil {
		return data, false
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &named) == nil && deny[named.Name] {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == len(tools) {
		return data, false
	}
	if !replaceField(result, "tools", kept) || !replaceField(msg, "result", result) {
		return data, false
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return data, false
	}
	return out, true
}

func isElicitationCreate(data []byte) bool {
	var msg struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(data, &msg) == nil && msg.Method == "elicitation/create"
}

// sseData joins an event's data fields. MCP sends single-line JSON, but the
// SSE spec allows a value split across several data lines.
func sseData(event []string) []byte {
	var data []string
	for _, line := range event {
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	return []byte(strings.Join(data, "\n"))
}

// writeEvent re-emits one SSE event: verbatim with newData nil, otherwise
// with the data payload replaced and non-data lines (event:, id:) kept.
func writeEvent(dst io.Writer, event []string, newData []byte) error {
	var b strings.Builder
	for _, line := range event {
		if newData != nil && strings.HasPrefix(line, "data:") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if newData != nil {
		b.WriteString("data: ")
		b.Write(newData)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, err := io.WriteString(dst, b.String())
	return err
}

func baseMediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}
