package daemon

import (
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/okedeji/agentcage/internal/llmgateway"
	"github.com/okedeji/agentcage/internal/mcpgateway"
	"github.com/okedeji/agentcage/internal/telemetry"
)

// buildTrace assembles a run's trace: the run span is the root, each agent
// that made an LLM call gets a span under it (widened to cover its calls), and
// each parent-to-sub-agent call gets a sub_agent span. Grouping LLM calls by
// agent gives the tree its shape without the gateway reporting the full call
// graph.
func buildTrace(runID string, start, end time.Time, calls []llmgateway.CallEvent, subCalls []mcpgateway.SubCallEvent) *telemetry.Trace {
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
	for _, s := range subCalls {
		root.Children = append(root.Children, &telemetry.Span{
			Name:       "agentcage.sub_agent.run",
			Start:      time.Unix(0, s.StartUnixNano),
			End:        time.Unix(0, s.EndUnixNano),
			Attributes: map[string]any{"edge": s.Edge, "tool": s.Tool},
		})
	}
	sort.SliceStable(root.Children, func(i, j int) bool { return root.Children[i].Start.Before(root.Children[j].Start) })
	return &telemetry.Trace{RunID: runID, Root: root}
}

// totalTokens sums every metered call's prompt and completion tokens.
func totalTokens(calls []llmgateway.CallEvent) int64 {
	var n int64
	for _, c := range calls {
		n += c.PromptTokens + c.CompletionTokens
	}
	return n
}

// handleRunTrace serves a run's stored trace JSON. A run with no trace (no LLM
// call made, or history off) is a 404.
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
