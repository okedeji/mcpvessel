package mcpgateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SubCallEvent is one parent-to-sub-agent tools/call, logged so the daemon
// can add a sub-agent span to the run's trace. Times are the gateway clock's
// unix nanos.
type SubCallEvent struct {
	Edge          string `json:"edge"`
	Tool          string `json:"tool"`
	StartUnixNano int64  `json:"start_unix_nano"`
	EndUnixNano   int64  `json:"end_unix_nano"`
}

// SubCallRecord is a sub-agent call's full payload, logged only when
// recording.
type SubCallRecord struct {
	Edge          string `json:"edge"`
	Tool          string `json:"tool"`
	Args          []byte `json:"args,omitempty"`
	Response      []byte `json:"response,omitempty"`
	StartUnixNano int64  `json:"start_unix_nano"`
	EndUnixNano   int64  `json:"end_unix_nano"`
}

// Log prefixes for the gateway's per-call stdout lines, read by the daemon at
// finish.
const (
	SubCallLogPrefix   = "AGENTCAGE_SUBCALL "
	SubReplayLogPrefix = "AGENTCAGE_SUBREPLAY "
)

// Hooks are the gateway's per-call observation callbacks. Call fires for
// every sub-agent tools/call; Payload only when the run records.
type Hooks struct {
	Call    func(SubCallEvent)
	Payload func(SubCallRecord)
}

// SetHooks wires the per-call observation callbacks.
func (g *Gateway) SetHooks(h Hooks) {
	g.recordCall = h.Call
	g.recordPayload = h.Payload
}

// WriteSubCallLine and WriteSubReplayLine emit one prefixed JSON line.
func WriteSubCallLine(w io.Writer, e SubCallEvent)    { writeJSONLine(w, SubCallLogPrefix, e) }
func WriteSubReplayLine(w io.Writer, r SubCallRecord) { writeJSONLine(w, SubReplayLogPrefix, r) }

func writeJSONLine(w io.Writer, prefix string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, prefix+string(b))
}

// ParseSubCallLines and ParseSubReplayLines return every matching log line,
// in order.
func ParseSubCallLines(logs string) []SubCallEvent {
	var out []SubCallEvent
	for _, line := range strings.Split(logs, "\n") {
		s, ok := strings.CutPrefix(strings.TrimSpace(line), SubCallLogPrefix)
		if !ok {
			continue
		}
		var e SubCallEvent
		if json.Unmarshal([]byte(s), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func ParseSubReplayLines(logs string) []SubCallRecord {
	var out []SubCallRecord
	for _, line := range strings.Split(logs, "\n") {
		s, ok := strings.CutPrefix(strings.TrimSpace(line), SubReplayLogPrefix)
		if !ok {
			continue
		}
		var r SubCallRecord
		if json.Unmarshal([]byte(s), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// parseToolsCall extracts the tool name and raw arguments from a tools/call
// request. A batch or any other method is not recorded as a sub-agent call.
func parseToolsCall(body []byte) (tool string, args []byte, ok bool) {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || req.Method != "tools/call" {
		return "", nil, false
	}
	return req.Params.Name, req.Params.Arguments, true
}

func (g *Gateway) recordSubCall(edge, tool string, args []byte, start, end time.Time, captured *bytes.Buffer) {
	if g.recordCall != nil {
		g.recordCall(SubCallEvent{Edge: edge, Tool: tool, StartUnixNano: start.UnixNano(), EndUnixNano: end.UnixNano()})
	}
	if captured != nil && g.recordPayload != nil {
		g.recordPayload(SubCallRecord{Edge: edge, Tool: tool, Args: args, Response: captured.Bytes(), StartUnixNano: start.UnixNano(), EndUnixNano: end.UnixNano()})
	}
}

// captureWriter tees response bytes into buf. It forwards Flush so a streamed
// SSE response still reaches the client immediately.
type captureWriter struct {
	http.ResponseWriter
	buf *bytes.Buffer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.buf.Write(p)
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
