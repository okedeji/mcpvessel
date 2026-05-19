package assessment

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/okedeji/agentcage/internal/cagefile"
	"github.com/okedeji/agentcage/internal/gateway"
)

// Planner calls the LLM to decide what cages to spawn next.
type Planner struct {
	client *gateway.Client
}

func NewPlanner(client *gateway.Client) *Planner {
	return &Planner{client: client}
}

const coordinatorSystemPrompt = `You coordinate an autonomous penetration test. Each iteration you receive a state snapshot and decide what to test next by spawning short-lived agents ("cages").

STATE SCHEMA:
- target: {host, ports, paths} the authorized scope (single host per assessment)
- goal: one paragraph committing to what this assessment is for. Generated up front from operator guidance and (when the human gate is enabled) reviewed by the operator before exploitation. Treat as a guardrail — do NOT propose actions whose intent falls outside it.
- agent_capabilities.exploitation: ["sqli","xss-fuzzer","idor-enum",...] tools the agent has loaded for exploitation. Free-text names chosen by the agent author. Treat them as a resume of what the agent can do — pick descriptive vuln_class labels for your actions and the agent dispatches each action to whichever loaded tool matches.
- guidance: operator-supplied direction. attack_surface.endpoints/api_specs name specific things to test; attack_surface.limit_to_listed=true means ONLY test those, ignore other discovery findings. attack_strategy.context is free-text background ("Django app, just rewrote OAuth"); attack_strategy.known_weaknesses are hints. Weight your planning toward these.
- findings[]: {id, kind, title, severity, vuln_class, endpoint, status, chain_depth}. kind="discovery" findings map attack surface (endpoints worth testing) and carry an empty vuln_class. kind="vulnerability" findings are confirmed issues; their vuln_class names the category.
- vuln_class: free-text label you pick when emitting actions. Use conventional names where they fit (sqli, xss, idor, info_disclosure, auth_bypass, ssrf, rce, csrf, headers, file_upload); invent a new label when the case is unusual. The agent reads these labels to dispatch to its loaded tools, so descriptive matters more than canonical.
- coverage: {host: [vuln_classes_already_tested]} what has been tested. Do not re-test these combinations.
- cages_completed[]: {cage_type, scope, vuln_class, objective, outcome, error, findings_count} prior cages and whether they succeeded
- tokens_used / token_budget: LLM token consumption. When tokens_used > 80% of token_budget, stop exploitation and set done=true.
- time_elapsed / time_limit: wall clock. If time_elapsed approaches time_limit, set done=true.
- iteration / max_iterations: loop position. You will not be called after max_iterations.

RESPONSE FORMAT (JSON only):
{
  "done": false,
  "reason": "one sentence: why these actions, or why done",
  "actions": [
    {
      "type": "exploitation",
      "scope": {"host": "target.example.com", "ports": ["443"], "paths": ["/api"]},
      "vuln_class": "sqli",
      "objective": "test /api/users endpoint for SQL injection via the id parameter",
      "priority": 1
    }
  ]
}

CAGE TYPES:
- exploitation: tests one endpoint for one vuln class. Also use for deeper testing on existing findings (e.g. SQLi found on /api, now try data extraction or privilege escalation). Set finding_id when going deeper on a specific finding.
- validator: independently confirms a finding is real. Requires finding_id. Do NOT use for testing new endpoints.

RULES:
1. If agent_capabilities.exploitation is empty, the agent has no exploitation tools loaded — set done=true immediately.
2. Check coverage before planning. If coverage[host] already includes a vuln class, skip it.
3. Prioritize: auth endpoints, admin panels, API routes, file upload, anything accepting user input.
4. Each action needs a specific objective the agent can act on. "test for SQLi" is too vague. "test /api/users?id= for error-based SQL injection" is actionable.
5. If a cage failed (outcome=failed, error set), decide whether to retry with a different approach or move on.
6. Maximum 10 actions per response.
7. Set done=true when coverage is sufficient, budget is low, or time is short.`

// PlanNextActions sends the coordinator state to the LLM and returns
// structured decisions about what cages to spawn.
func (p *Planner) PlanNextActions(ctx context.Context, state CoordinatorState) (CoordinatorDecision, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return CoordinatorDecision{}, fmt.Errorf("marshaling coordinator state: %w", err)
	}

	resp, err := p.client.ChatCompletion(ctx, "coordinator-"+state.AssessmentID, state.AssessmentID, state.TokenBudget, gateway.LLMRequest{
		Messages: []gateway.LLMMessage{
			{Role: "system", Content: coordinatorSystemPrompt},
			{Role: "user", Content: string(stateJSON)},
		},
	})
	if err != nil {
		return CoordinatorDecision{}, fmt.Errorf("calling LLM for coordinator decision: %w", err)
	}

	if len(resp.Choices) == 0 {
		return CoordinatorDecision{}, fmt.Errorf("LLM returned no choices")
	}

	content := resp.Choices[0].Message.Content
	var decision CoordinatorDecision
	if err := json.Unmarshal([]byte(content), &decision); err != nil {
		return CoordinatorDecision{}, fmt.Errorf("parsing coordinator decision from LLM response: %w (response: %s)", err, truncate(content, 200))
	}

	if err := validateDecision(decision); err != nil {
		return CoordinatorDecision{}, fmt.Errorf("invalid coordinator decision: %w", err)
	}

	return decision, nil
}

func validateDecision(d CoordinatorDecision) error {
	if d.Done {
		return nil
	}

	if len(d.Actions) == 0 {
		return fmt.Errorf("decision is not done but has no actions")
	}

	if len(d.Actions) > 10 {
		return fmt.Errorf("too many actions: %d (max 10)", len(d.Actions))
	}

	for i, a := range d.Actions {
		switch a.Type {
		case "exploitation", "validator":
		default:
			return fmt.Errorf("action %d: invalid type %q", i, a.Type)
		}

		if a.Scope.Host == "" {
			return fmt.Errorf("action %d: scope must have a host", i)
		}

		if a.Objective == "" {
			return fmt.Errorf("action %d: objective is required", i)
		}

		if a.Type == "validator" && a.FindingID == "" {
			return fmt.Errorf("action %d: validator cages require a finding_id", i)
		}
	}

	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

const goalSystemPrompt = `You are framing an autonomous penetration test before any cage runs. Given the operator's guidance and target, write one paragraph (3-5 sentences) that this assessment commits to:

- What scope and surface to focus on (be specific about which routes, components, or behaviors).
- What classes of issues are in scope (sqli, idor, info disclosure, etc.).
- Whether probes should stay read-only or are allowed to attempt state-changing operations.
- Anything the operator's guidance signals should NOT be touched.

The goal is a guardrail the coordinator and operator both reference later. Be concrete; avoid generic security boilerplate. No markdown, no headers — plain prose only.`

// GenerateGoal produces the assessment-wide commitment that anchors
// the discovery objective and the exploitation guardrail. Runs once
// at workflow start before any cage spawns.
func (p *Planner) GenerateGoal(ctx context.Context, assessmentID string, target string, guidance *Guidance, tokenBudget int64) (string, error) {
	type input struct {
		Target   string    `json:"target"`
		Guidance *Guidance `json:"guidance,omitempty"`
	}
	body, err := json.Marshal(input{Target: target, Guidance: guidance})
	if err != nil {
		return "", fmt.Errorf("marshaling goal input: %w", err)
	}

	resp, err := p.client.ChatCompletion(ctx, "goal-"+assessmentID, assessmentID, tokenBudget, gateway.LLMRequest{
		Messages: []gateway.LLMMessage{
			{Role: "system", Content: goalSystemPrompt},
			{Role: "user", Content: string(body)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("calling LLM for goal generation: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices for goal generation")
	}
	goal := resp.Choices[0].Message.Content
	if goal == "" {
		return "", fmt.Errorf("LLM returned empty goal")
	}
	return goal, nil
}

const planProposalSystemPrompt = `You propose a concrete exploitation plan an operator will review before any exploit cage spawns. You are given the assessment's goal, the operator's guidance, the discovery findings, and the agent's loaded capabilities.

Produce JSON only:
{
  "summary": "one paragraph summarizing the attack surface discovery found and what the plan covers",
  "actions": [
    {
      "type": "exploitation",
      "scope": {"host": "...", "ports": ["..."], "paths": ["..."]},
      "vuln_class": "sqli",
      "objective": "test /api/users?id= for error-based SQL injection",
      "priority": 1
    }
  ],
  "estimated_cages": 8,
  "estimated_tokens": 120000,
  "notes": "anything risky the operator should weigh"
}

RULES:
1. Anchor every action on the goal — do not propose actions outside its intent.
2. Only pick vuln_class labels that match the agent's loaded exploitation tools (free-text resume).
3. Each action's objective must be concrete (a specific endpoint and how to probe it), not generic.
4. estimated_cages = len(actions). estimated_tokens is a rough total (use 10-20K per cage as a heuristic).
5. If the operator provided feedback on a prior proposal, treat it as revisions and adjust accordingly. Mention what changed in notes.
6. If discovery found nothing actionable or the agent has no exploitation tools, return an empty actions array with notes explaining why.`

// PlanProposalInput bundles everything GenerateExploitationPlan needs.
// Bundled so the signature does not churn as the planner learns to
// reason about more context.
type PlanProposalInput struct {
	AssessmentID     string
	Goal             string                     `json:"goal"`
	Guidance         *Guidance                  `json:"guidance,omitempty"`
	Findings         []FindingSummary           `json:"findings"`
	Capabilities     cagefile.AgentCapabilities `json:"agent_capabilities"`
	OperatorFeedback string                     `json:"operator_feedback,omitempty"`
	TokenBudget      int64                      `json:"-"`
}

// GenerateExploitationPlan asks the LLM to propose a concrete plan
// the operator can review. When OperatorFeedback is non-empty, the
// prompt treats it as revisions on a prior proposal.
func (p *Planner) GenerateExploitationPlan(ctx context.Context, in PlanProposalInput) (PlanProposal, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return PlanProposal{}, fmt.Errorf("marshaling plan input: %w", err)
	}

	resp, err := p.client.ChatCompletion(ctx, "plan-"+in.AssessmentID, in.AssessmentID, in.TokenBudget, gateway.LLMRequest{
		Messages: []gateway.LLMMessage{
			{Role: "system", Content: planProposalSystemPrompt},
			{Role: "user", Content: string(body)},
		},
	})
	if err != nil {
		return PlanProposal{}, fmt.Errorf("calling LLM for plan proposal: %w", err)
	}
	if len(resp.Choices) == 0 {
		return PlanProposal{}, fmt.Errorf("LLM returned no choices for plan proposal")
	}

	content := resp.Choices[0].Message.Content
	var raw struct {
		Summary         string              `json:"summary"`
		Actions         []CoordinatorAction `json:"actions"`
		EstimatedCages  int32               `json:"estimated_cages"`
		EstimatedTokens int64               `json:"estimated_tokens"`
		Notes           string              `json:"notes,omitempty"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return PlanProposal{}, fmt.Errorf("parsing plan proposal from LLM response: %w (response: %s)", err, truncate(content, 200))
	}

	for i, a := range raw.Actions {
		if a.Type == "" {
			raw.Actions[i].Type = "exploitation"
		}
		if a.Scope.Host == "" {
			return PlanProposal{}, fmt.Errorf("plan action %d: scope.host is required", i)
		}
		if a.Objective == "" {
			return PlanProposal{}, fmt.Errorf("plan action %d: objective is required", i)
		}
	}

	return PlanProposal{
		Goal:            in.Goal,
		Summary:         raw.Summary,
		Actions:         raw.Actions,
		EstimatedCages:  raw.EstimatedCages,
		EstimatedTokens: raw.EstimatedTokens,
		Notes:           raw.Notes,
	}, nil
}
