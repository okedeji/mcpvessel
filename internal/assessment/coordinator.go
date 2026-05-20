package assessment

import (
	"time"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/cagefile"
	"github.com/okedeji/agentcage/internal/findings"
)

// CoordinatorState is the snapshot of assessment progress sent to the LLM
// each iteration. The coordinator reasons about what has been tested, what
// was found, and decides what to test next.
type CoordinatorState struct {
	AssessmentID   string              `json:"assessment_id"`
	Target         cage.Scope          `json:"target"`
	Iteration      int                 `json:"iteration"`
	MaxIterations  int                 `json:"max_iterations"`
	Findings       []FindingSummary    `json:"findings"`
	CagesCompleted []CageSummary       `json:"cages_completed"`
	Coverage       map[string][]string `json:"coverage"`
	TokensUsed     int64               `json:"tokens_used"`
	TokenBudget    int64               `json:"token_budget"`
	TimeElapsed    time.Duration       `json:"time_elapsed"`
	TimeLimit      time.Duration       `json:"time_limit"`
	// AgentCapabilities advertises what the agent has loaded. Discovery
	// and Validation are phase markers; Exploitation is a free-text list
	// of tool names the LLM reads as a resume. The LLM decides what
	// actions to plan based on what's listed here.
	AgentCapabilities cagefile.AgentCapabilities `json:"agent_capabilities"`
	// Guidance is operator-supplied direction. AttackSurface flags
	// specific endpoints/specs to test (with LimitToListed restricting
	// to only those). AttackStrategy carries free-text context and
	// known-weakness hints. Weight planning toward these.
	Guidance *Guidance `json:"guidance,omitempty"`
	// Goal is the assessment-wide commitment the orchestrator
	// generated before discovery from the operator's guidance + target.
	// Treat as a guardrail: don't propose actions whose intent falls
	// outside it. Empty during the auto-approve path's first iteration
	// only if goal generation failed; the workflow refuses to start
	// exploitation without a goal.
	Goal string `json:"goal,omitempty"`
}

// FindingSummary is a compact representation of a finding for the
// coordinator. The full finding has large evidence payloads; the
// coordinator only needs metadata to reason about coverage.
type FindingSummary struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	VulnClass  string `json:"vuln_class,omitempty"`
	Endpoint   string `json:"endpoint"`
	Status     string `json:"status"`
	ChainDepth int32  `json:"chain_depth"`
}

// CageSummary records what a completed cage attempted and found.
// The coordinator uses Outcome and Error to decide whether to retry,
// skip, or stop the assessment.
type CageSummary struct {
	CageID    string `json:"cage_id"`
	CageType  string `json:"cage_type"`
	Scope     string `json:"scope"`
	VulnClass string `json:"vuln_class"`
	Objective string `json:"objective"`
	Outcome   string `json:"outcome"`
	Error     string `json:"error,omitempty"`
	Findings  int    `json:"findings_count"`
}

// CoordinatorDecision is the structured response from the LLM coordinator.
type CoordinatorDecision struct {
	Done    bool                `json:"done"`
	Reason  string              `json:"reason"`
	Actions []CoordinatorAction `json:"actions"`
}

// CoordinatorAction describes a single cage to spawn.
type CoordinatorAction struct {
	Type             string     `json:"type"`
	Scope            cage.Scope `json:"scope"`
	VulnClass        string     `json:"vuln_class"`
	FindingID        string     `json:"finding_id,omitempty"`
	Objective        string     `json:"objective"`
	Priority         int        `json:"priority"`
	RecommendedJudge bool       `json:"recommended_judge,omitempty"`
}

// SummarizeFindings converts full findings to coordinator-friendly summaries.
func SummarizeFindings(ff []findings.Finding) []FindingSummary {
	summaries := make([]FindingSummary, len(ff))
	for i, f := range ff {
		summaries[i] = FindingSummary{
			ID:         f.ID,
			Kind:       string(f.Kind),
			Title:      f.Title,
			Severity:   f.Severity.String(),
			VulnClass:  f.VulnClass,
			Endpoint:   f.Endpoint,
			Status:     f.Status.String(),
			ChainDepth: f.ChainDepth,
		}
	}
	return summaries
}

// UpdateCoverage tracks which vuln classes have been tested per endpoint.
func UpdateCoverage(coverage map[string][]string, actions []CoordinatorAction) map[string][]string {
	if coverage == nil {
		coverage = make(map[string][]string)
	}
	for _, a := range actions {
		key := a.Scope.Host
		if key == "" || a.VulnClass == "" {
			continue
		}
		found := false
		for _, existing := range coverage[key] {
			if existing == a.VulnClass {
				found = true
				break
			}
		}
		if !found {
			coverage[key] = append(coverage[key], a.VulnClass)
		}
	}
	return coverage
}
