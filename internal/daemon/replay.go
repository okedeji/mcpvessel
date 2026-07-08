package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/llmgateway"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/mcpgateway"
	"github.com/okedeji/agentcage/internal/replay"
	"github.com/okedeji/agentcage/internal/runtime"
)

// writeReplay assembles and writes a recording run's .replay artifact: the
// run's input and output plus the gateway's captured call payloads. It must
// run before teardown removes the gateway. Best-effort: a recording that
// cannot be written warns and never fails the run.
func (d *Daemon) writeReplay(runID string, b locate.Result, req RunRequest, result string, callErr error, started time.Time) {
	records, _ := runtime.RunReplay(context.Background(), runID)
	subRecords, _ := runtime.RunSubagentReplay(context.Background(), runID)
	rec := &replay.Recording{
		Version:   replay.Version,
		AgentRef:  b.Display,
		RunID:     runID,
		Input:     replay.Input{Tool: req.Tool, Args: req.Args},
		Events:    replayEvents(records, subRecords),
		StartedAt: started,
		EndedAt:   nowFunc(),
		Result:    replayResult(result, callErr),
	}
	if m, err := bundle.ReadManifest(b.Path); err == nil {
		rec.ManifestHash = m.FilesHash
	}
	if err := replay.Write(rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: writing replay for %s: %v\n", runID, err)
	}
}

// replayEvents merges LLM call records and sub-agent records into one list,
// ordered by start and numbered by seq. A captured body is embedded raw when
// it is JSON, as a JSON string otherwise (a streamed response).
func replayEvents(records []llmgateway.CallRecord, subRecords []mcpgateway.SubCallRecord) []replay.Event {
	timed := make([]struct {
		start int64
		ev    replay.Event
	}, 0, len(records)+len(subRecords))
	for _, r := range records {
		typ := replay.EventLLMComplete
		if r.Streamed {
			typ = replay.EventLLMStream
		}
		timed = append(timed, struct {
			start int64
			ev    replay.Event
		}{r.StartUnixNano, replay.Event{
			Type:         typ,
			Request:      replay.RawOrString(r.Request),
			Response:     replay.RawOrString(r.Response),
			TokensIn:     r.PromptTokens,
			TokensOut:    r.CompletionTokens,
			CostMicroUSD: r.CostMicroUSD,
			TUnixNano:    r.StartUnixNano,
		}})
	}
	for _, r := range subRecords {
		timed = append(timed, struct {
			start int64
			ev    replay.Event
		}{r.StartUnixNano, replay.Event{
			Type:      "subagent." + r.Edge + "." + r.Tool,
			Request:   replay.RawOrString(r.Args),
			Response:  replay.RawOrString(r.Response),
			TUnixNano: r.StartUnixNano,
		}})
	}
	sort.SliceStable(timed, func(i, j int) bool { return timed[i].start < timed[j].start })

	out := make([]replay.Event, 0, len(timed))
	for i, t := range timed {
		ev := t.ev
		ev.Seq = i
		out = append(out, ev)
	}
	return out
}

func replayResult(result string, callErr error) replay.Result {
	if callErr != nil {
		return replay.Result{Status: "failed", Error: callErr.Error()}
	}
	return replay.Result{Output: result, Status: "succeeded"}
}

// handleRunReplay serves a run's .replay artifact; a run that did not record
// is a 404.
func (d *Daemon) handleRunReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, err := replay.Path(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "no replay for run "+id+" (was it recorded with 'agentcage replay record'?)")
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, f)
}
