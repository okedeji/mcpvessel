package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

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
}

func (r RunRequest) String() string {
	return fmt.Sprintf("RunRequest{Ref:%q Tool:%q Args:%d env:%d secrets:%d}",
		r.Ref, r.Tool, len(r.Args), len(r.Env), len(r.Secrets))
}

func (r RunRequest) GoString() string { return r.String() }

// runFrame is one line of the POST /run NDJSON stream: a chunk of the run's logs
// (build progress and the agent's stderr), the final result, or a terminal
// error. The CLI prints log frames as they arrive and the result at the end.
type runFrame struct {
	Type string `json:"type"` // "log" | "result" | "error"
	Data string `json:"data"`
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

	b, err := locate.Bundle(r.Context(), req.Ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	stream := newRunStream(w)
	session, err := d.boot(r.Context(), runtime.RunInput{
		BundlePath:  b.Path,
		Name:        b.Name,
		Budget:      req.Budget,
		Env:         req.Env,
		Secrets:     req.Secrets,
		Resources:   req.Resources,
		NoCache:     req.NoCache,
		Interaction: env.InteractionOneShot,
		Stdout:      io.Discard,
		Stderr:      stream.logWriter(),
	}, b.Display)
	if err != nil {
		stream.frame("error", err.Error())
		return
	}

	result, callErr := session.Call(r.Context(), req.Tool, req.Args)
	status := history.StatusSucceeded
	if callErr != nil {
		status = history.StatusFailed
	}
	// Record before dropRuns tears the gateway down, so the run's final spend is
	// still readable; recordFinish escalates a failure to over_budget from it.
	d.recordFinish(session.RunID(), status, callErr)
	d.dropRuns([]*runtime.Session{session})
	if callErr != nil {
		stream.frame("error", callErr.Error())
		return
	}
	stream.frame("result", result)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(runFrame{Type: typ, Data: data})
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
