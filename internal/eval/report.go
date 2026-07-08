package eval

import (
	"fmt"
	"strings"
	"time"
)

// CaseResult is the outcome of one eval case. JudgeScore is nil unless the
// case ran a judge; a run that errored before producing output has no score.
type CaseResult struct {
	Name         string   `json:"name"`
	Passed       bool     `json:"passed"`
	Failures     []string `json:"failures,omitempty"`
	CostMicroUSD int64    `json:"cost_micro_usd"`
	DurationMS   int64    `json:"duration_ms"`
	JudgeScore   *float64 `json:"judge_score,omitempty"`
	JudgeReason  string   `json:"judge_reason,omitempty"`

	// judgeCostMicroUSD is the judge's own spend, summed into the report
	// footer but never counted against max_cost_usd: that ceiling measures
	// the agent, not the operator's choice to grade it.
	judgeCostMicroUSD int64
}

// Duration is the case's tool-call wall time.
func (r CaseResult) Duration() time.Duration {
	return time.Duration(r.DurationMS) * time.Millisecond
}

// Report is a whole suite run: per-case results, aggregate counts, the mean
// judge score across judged cases, and agent and judge spend.
type Report struct {
	Cases             []CaseResult `json:"cases"`
	Passed            int          `json:"passed"`
	Failed            int          `json:"failed"`
	JudgeScore        *float64     `json:"judge_score,omitempty"`
	JudgeCount        int          `json:"judge_count"`
	CostMicroUSD      int64        `json:"cost_micro_usd"`
	JudgeCostMicroUSD int64        `json:"judge_cost_micro_usd"`
	ElapsedMS         int64        `json:"elapsed_ms"`
}

// Elapsed is the suite's wall time.
func (r Report) Elapsed() time.Duration {
	return time.Duration(r.ElapsedMS) * time.Millisecond
}

// FormatUSD renders micro-USD as dollars, keeping micro precision for small
// values ($0.000007 does not round away to $0.000) and trimming past two
// decimals so a round amount stays $5.00.
func FormatUSD(microUSD int64) string {
	s := fmt.Sprintf("%d.%06d", microUSD/1_000_000, microUSD%1_000_000)
	s = strings.TrimRight(s, "0")
	switch dot := strings.IndexByte(s, '.'); {
	case dot == len(s)-1:
		s += "00"
	case len(s)-dot-1 < 2:
		s += "0"
	}
	return "$" + s
}
