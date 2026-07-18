package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/history"
	"github.com/okedeji/mcpvessel/internal/locate"
	"github.com/okedeji/mcpvessel/internal/runtime"
)

// RunRequest is the POST /run body: one boot, one tool call, one teardown.
// The CLI resolves and authorizes the tool before sending.
//
// String and GoString redact Secrets and Env so a logged request never leaks
// them; JSON marshaling stays real because it is the wire format.
type RunRequest struct {
	Ref    string            `json:"ref"`
	Tool   string            `json:"tool"`
	Args   map[string]any    `json:"args,omitempty"`
	Budget int64             `json:"budget,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
	// Secrets is scoped: "" is the broadcast pool, any other key grants only
	// the agent with that short name (run name or USES alias).
	Secrets   runtime.ScopedSecrets `json:"secrets,omitempty"`
	Resources config.Cap            `json:"resources,omitempty"`
	NoCache   bool                  `json:"no_cache,omitempty"`
	Record    bool                  `json:"record,omitempty"`
	Egress    map[string][]string   `json:"egress,omitempty"` // scoped per-agent operator override
	// TimeoutSeconds bounds the tool call, not the boot: a first-use image
	// build can take minutes and must not count against the deadline. Zero
	// means no deadline.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
}

func (r RunRequest) String() string {
	return fmt.Sprintf("RunRequest{Ref:%q Tool:%q Args:%d env:%d secrets:%d}",
		r.Ref, r.Tool, len(r.Args), len(r.Env), len(r.Secrets))
}

func (r RunRequest) GoString() string { return r.String() }

// runFrame is one line of the POST /run NDJSON stream. Terminal frames (result
// and error) also carry the run's LLM spend and the call's wall time: the wire
// is the reliable channel for a one-shot's cost, since the best-effort history
// store may never hold it.
type runFrame struct {
	Type         string `json:"type"` // "log" | "run_id" | "result" | "error"
	Data         string `json:"data"`
	CostMicroUSD int64  `json:"cost_micro_usd,omitempty"`
	CallMS       int64  `json:"call_ms,omitempty"`
}

// handleRun boots a one-shot run, streams its logs, and ends with the tool
// result or an error. It binds to the request context: a client that gives up
// cancels the boot and the call. Once the stream opens the status is 200 and
// failure rides in a frame, because the boot can fail after the first log line
// is already on the wire.
func (d *Daemon) handleRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required (the CLI resolves MAIN or an explicit tool)")
		return
	}
	if req.TimeoutSeconds < 0 {
		writeError(w, http.StatusBadRequest, "timeout_seconds must not be negative")
		return
	}

	b, err := locate.Bundle(r.Context(), req.Ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	stream := newRunStream(w)
	started := nowFunc()
	session, err := d.boot(r.Context(), runtime.RunInput{
		BundlePath:  b.Path,
		Name:        b.Name,
		Ref:         req.Ref,
		Budget:      req.Budget,
		Env:         req.Env,
		Secrets:     req.Secrets,
		Resources:   req.Resources,
		NoCache:     req.NoCache,
		Record:      req.Record,
		EgressAllow: egress.HostsFor(req.Egress, b.Name),
		Interaction: env.InteractionOneShot,
		Stdout:      io.Discard,
		Stderr:      stream.logWriter(),
	}, b.Display)
	if err != nil {
		stream.frame("error", err.Error())
		return
	}
	stream.frame("run_id", session.RunID())

	// The deadline and the timing below cover the tool call alone: a first-use
	// image build dwarfs any call and would swamp both.
	callCtx := r.Context()
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(callCtx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	// Nil args must go out as {}, not null: a strict server rejects null for a
	// tool whose schema is an (empty) object.
	callArgs := req.Args
	if callArgs == nil {
		callArgs = map[string]any{}
	}
	callStart := nowFunc()
	// While the call runs, forward any egress hold to the client as an approval
	// frame so an attached run/call can prompt the operator inline.
	stopWatch := d.watchEgressPending(session.RunID(), stream)
	result, callErr := session.Call(callCtx, req.Tool, callArgs)
	stopWatch()
	callErr = enrichEgressError(callErr, d.denials.hosts(session.RunID()))
	callMS := nowFunc().Sub(callStart).Milliseconds()
	status := history.StatusSucceeded
	if callErr != nil {
		status = history.StatusFailed
	}
	// All gateway reads (replay payloads, then spend) must happen before
	// dropRuns tears the gateway down.
	if req.Record {
		d.writeReplay(session.RunID(), b, req, result, callErr, started)
	}
	cost := d.finish(session.RunID(), b.Display, status, callErr)
	d.dropRuns([]*runtime.Session{session})
	// The cost rides the error frame too: a run that overspent and failed
	// still reports what it burned.
	if callErr != nil {
		stream.endFrame("error", callErr.Error(), cost, callMS)
		return
	}
	stream.endFrame("result", result, cost, callMS)
}

// runStream writes runFrames to the response, flushing each. Mutex-guarded:
// the agent's stderr (a copy goroutine) and the result write race onto the
// same response.
type runStream struct {
	mu      sync.Mutex
	enc     *json.Encoder
	flusher http.Flusher
}

func newRunStream(w http.ResponseWriter) *runStream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return &runStream{enc: json.NewEncoder(w), flusher: flusher}
}

func (s *runStream) frame(typ, data string) {
	s.write(runFrame{Type: typ, Data: data})
}

func (s *runStream) endFrame(typ, data string, costMicroUSD, callMS int64) {
	s.write(runFrame{Type: typ, Data: data, CostMicroUSD: costMicroUSD, CallMS: callMS})
}

func (s *runStream) write(f runFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(f)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *runStream) logWriter() io.Writer { return logFramer{s} }

// logFramer turns each write of an agent's stderr into a log frame.
type logFramer struct{ s *runStream }

func (l logFramer) Write(p []byte) (int, error) {
	l.s.frame("log", string(p))
	return len(p), nil
}
