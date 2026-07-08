// Package telemetry holds a run's trace model: the span tree the daemon
// writes into history and `agentcage trace` renders.
package telemetry

import "time"

// Span is one node in a run's trace: a run holds an agent per reasoning cage,
// an agent holds its LLM calls. Attributes carry measured facts (model,
// tokens, cost).
type Span struct {
	Name       string         `json:"name"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Children   []*Span        `json:"children,omitempty"`
}

// Duration is the span's elapsed wall time, zero for an unfinished or
// zero-width span.
func (s *Span) Duration() time.Duration {
	if !s.End.After(s.Start) {
		return 0
	}
	return s.End.Sub(s.Start)
}

// Trace is one run's whole span tree, rooted at the run span.
type Trace struct {
	RunID string `json:"run_id"`
	Root  *Span  `json:"root"`
}
