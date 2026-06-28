// Package telemetry holds a run's trace model: the span tree the daemon builds
// from a run's recorded events and `agentcage trace` renders. It is the shared
// shape between the daemon that writes it into the history, the CLI that prints
// it, and the OTLP exporter that ships it to a backend, so none of them spells
// the schema twice. The model is exporter-agnostic; mapping it onto OpenTelemetry
// spans is the exporter's job, not this package's.
package telemetry

import "time"

// Span is one node in a run's trace. Children nest under it: a run holds an agent
// per reasoning cage, an agent holds its LLM calls. Attributes carry the span's
// measured facts (model, tokens, cost). Times are absolute; Duration derives the
// elapsed wall time.
type Span struct {
	Name       string         `json:"name"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Children   []*Span        `json:"children,omitempty"`
}

// Duration is the span's elapsed wall time, zero when its end is not after its
// start (an unfinished or zero-width span).
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
