package daemon

import (
	"io"
	"net/http"
	"time"

	"github.com/okedeji/agentcage/internal/llmgateway"
	"github.com/okedeji/agentcage/internal/telemetry"
)

// buildTrace assembles a run's trace from its run window and the gateway's
// per-call events. The run span is the root; each agent that made a call gets a
// span under it, widened to cover its calls, and every call nests under its
// agent. Grouping by agent gives the tree its parent-to-child shape (the root and
// each sub-agent appear as their own node) without the gateway reporting the call
// graph.
func buildTrace(runID string, start, end time.Time, calls []llmgateway.CallEvent) *telemetry.Trace {
	root := &telemetry.Span{
		Name:       "agentcage.run",
		Start:      start,
		End:        end,
		Attributes: map[string]any{"run_id": runID},
	}
	agents := map[string]*telemetry.Span{}
	for _, c := range calls {
		ag := agents[c.Agent]
		if ag == nil {
			ag = &telemetry.Span{Name: "agent:" + c.Agent, Attributes: map[string]any{"agent": c.Agent}}
			agents[c.Agent] = ag
			root.Children = append(root.Children, ag)
		}
		cs, ce := time.Unix(0, c.StartUnixNano), time.Unix(0, c.EndUnixNano)
		ag.Children = append(ag.Children, &telemetry.Span{
			Name:  "agentcage.llm.call",
			Start: cs,
			End:   ce,
			Attributes: map[string]any{
				"model":             c.Model,
				"prompt_tokens":     c.PromptTokens,
				"completion_tokens": c.CompletionTokens,
				"cost_micro_usd":    c.CostMicroUSD,
			},
		})
		if ag.Start.IsZero() || cs.Before(ag.Start) {
			ag.Start = cs
		}
		if ce.After(ag.End) {
			ag.End = ce
		}
	}
	return &telemetry.Trace{RunID: runID, Root: root}
}

// totalTokens sums every metered call's prompt and completion tokens, the run's
// whole token spend for the history record.
func totalTokens(calls []llmgateway.CallEvent) int64 {
	var n int64
	for _, c := range calls {
		n += c.PromptTokens + c.CompletionTokens
	}
	return n
}

// handleRunTrace serves a run's stored trace JSON. A run with no trace (it made
// no LLM call, or history is off) is a 404.
func (d *Daemon) handleRunTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.hist == nil {
		writeError(w, http.StatusNotFound, "no run history")
		return
	}
	rec, found, err := d.hist.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found || rec.TraceJSON == "" {
		writeError(w, http.StatusNotFound, "no trace for run "+id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, rec.TraceJSON)
}
