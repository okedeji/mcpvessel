package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/history"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/runtime"
)

// RunRequest is the POST /run body: one boot, one tool call, one teardown, the
// daemon-side of `agentcage run` and `agentcage call`. The CLI resolves the
// tool and authorizes it before sending; the daemon executes and owns the run.
//
// Secrets and Env travel from the CLI over the daemon's local Unix socket.
// String and GoString redact both so a logged request never leaks them;
// MarshalJSON stays real because it is the wire format the daemon injects from.
type RunRequest struct {
	Ref       string            `json:"ref"`
	Tool      string            `json:"tool"`
	Args      map[string]any    `json:"args,omitempty"`
	Budget    int64             `json:"budget,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	Resources config.Cap        `json:"resources,omitempty"`
	NoCache   bool              `json:"no_cache,omitempty"`
	Record    bool              `json:"record,omitempty"`
	// TimeoutSeconds bounds the tool call, not the boot: the first run of an
	// agent builds its image, which can take minutes and must not count against
	// an eval case's max_duration_seconds. Zero means no deadline.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
}

func (r RunRequest) String() string {
	return fmt.Sprintf("RunRequest{Ref:%q Tool:%q Args:%d env:%d secrets:%d}",
		r.Ref, r.Tool, len(r.Args), len(r.Env), len(r.Secrets))
}

func (r RunRequest) GoString() string { return r.String() }

// runFrame is one line of the POST /run NDJSON stream: a chunk of the run's logs
// (build progress and the agent's stderr), the final result, or a terminal
// error. The CLI prints log frames as they arrive and the result at the end.
//
// The terminal result and error frames also carry the run's LLM spend and the
// tool call's wall time, so a client (the eval runner) reads a run's cost and
// duration off the wire without a follow-up query the best-effort history store
// might not answer. Log and run_id frames leave both zero; omitempty drops them.
type runFrame struct {
	Type         string `json:"type"` // "log" | "run_id" | "result" | "error"
	Data         string `json:"data"`
	CostMicroUSD int64  `json:"cost_micro_usd,omitempty"`
	CallMS       int64  `json:"call_ms,omitempty"`
}

// handleRun boots a one-shot run, streams its logs while it works, and ends with
// the tool result or an error. The run is held for its brief life so ps and stop
// see it, and released when the call returns or the client disconnects. It binds
// to the request context: a client that gives up cancels the boot and the call.
//
// Once the stream opens the status is 200 and success or failure rides in a
// frame, not the HTTP status, because the boot can fail after the first log line
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
		Budget:      req.Budget,
		Env:         req.Env,
		Secrets:     req.Secrets,
		Resources:   req.Resources,
		NoCache:     req.NoCache,
		Record:      req.Record,
		Interaction: env.InteractionOneShot,
		Stdout:      io.Discard,
		Stderr:      stream.logWriter(),
	}, b.Display)
	if err != nil {
		stream.frame("error", err.Error())
		return
	}
	// The client learns the run id up front, so `replay record` can fetch the
	// artifact afterward. run/call ignore the frame.
	stream.frame("run_id", session.RunID())

	// Time the tool call alone, not the boot above it: a first-use image build
	// dwarfs any call and would make an eval's per-case duration meaningless.
	// The deadline wraps only the call for the same reason.
	callCtx := r.Context()
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(callCtx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	callStart := nowFunc()
	result, callErr := session.Call(callCtx, req.Tool, req.Args)
	callMS := nowFunc().Sub(callStart).Milliseconds()
	status := history.StatusSucceeded
	if callErr != nil {
		status = history.StatusFailed
	}
	// All gateway reads (replay payloads, then spend) happen before dropRuns tears
	// the gateway down. writeReplay is a no-op unless this run recorded.
	if req.Record {
		d.writeReplay(session.RunID(), b, req, result, callErr, started)
	}
	cost := d.finish(session.RunID(), b.Display, status, callErr)
	d.dropRuns([]*runtime.Session{session})
	// The cost rides the error frame too: a case that overspent and failed still
	// reports the money it burned.
	if callErr != nil {
		stream.endFrame("error", callErr.Error(), cost, callMS)
		return
	}
	stream.endFrame("result", result, cost, callMS)
}

// runStream writes runFrames to the response, flushing each so the CLI sees logs
// as they happen rather than in one burst at the end. Its frame method is
// mutex-guarded because the agent's stderr (a copy goroutine) and the result
// write race onto the same response.
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

// endFrame writes a terminal frame carrying the run's cost and call duration
// alongside the result or error.
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
