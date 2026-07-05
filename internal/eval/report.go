package eval

import (
	"fmt"
	"time"
)

// CaseResult is the outcome of one eval case: whether it passed, the reasons it
// did not, and what it cost and took. JudgeScore is nil unless the case ran a
// judge; a run that errored before producing output has no score to record.
type CaseResult struct {
	Name         string   `json:"name"`
	Passed       bool     `json:"passed"`
	Failures     []string `json:"failures,omitempty"`
	CostMicroUSD int64    `json:"cost_micro_usd"`
	DurationMS   int64    `json:"duration_ms"`
	JudgeScore   *float64 `json:"judge_score,omitempty"`
	JudgeReason  string   `json:"judge_reason,omitempty"`

	// judgeCostMicroUSD is the judge's own spend for this case, summed into the
	// report footer. It is never counted against the case's max_cost_usd: that
	// ceiling measures the agent, not the operator's choice to grade it.
	judgeCostMicroUSD int64
}

// Duration is the case's tool-call wall time.
func (r CaseResult) Duration() time.Duration {
	return time.Duration(r.DurationMS) * time.Millisecond
}

// Report is a whole suite run: per-case results plus aggregate counts, the mean
// judge score across judged cases, and the money the agent and the judge spent.
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

// FormatUSD renders a micro-USD amount as a dollar figure at cent-plus
// precision, the scale an eval's per-case costs land at.
func FormatUSD(microUSD int64) string {
	return fmt.Sprintf("$%.3f", float64(microUSD)/1_000_000)
}
