package assessment

import (
	"context"
	"encoding/json"
	"fmt"

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
- target: {hosts, ports, paths} the authorized scope
- agent_capabilities.exploitation: ["sqli","xss",...] vuln classes the agent can test. You may ONLY request these.
- findings[]: {id, title, severity, vuln_class, endpoint, status, chain_depth} discovered so far
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
      "scope": {"hosts": ["target.example.com"], "ports": ["443"], "paths": ["/api"]},
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
1. Only request vuln classes from agent_capabilities.exploitation. If the list is empty, set done=true immediately.
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

		if len(a.Scope.Hosts) == 0 {
			return fmt.Errorf("action %d: scope must have at least one host", i)
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
