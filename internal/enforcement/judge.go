package enforcement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// JudgePayload is what the proxy sends to a judge endpoint for a
// single request. Wire format: the endpoint receives a POST body
// {"payloads": [JudgePayload, ...]} and must respond with
// {"results": [JudgeResult, ...]} — one result per payload, in order.
type JudgePayload struct {
	CageType     string            `json:"cage_type"`
	VulnClass    string            `json:"vuln_class"`
	AssessmentID string            `json:"assessment_id"`
	Method       string            `json:"method"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body"`
	// Objective is the per-cage instruction the coordinator wrote for
	// this cage (e.g. "test /api/users?id= for error-based SQLi"). Lets
	// the judge reason about what the cage is supposed to be doing.
	Objective string `json:"objective,omitempty"`
	// AgentReason is the per-request justification the agent supplied
	// via the X-Agentcage-Judge-Reason header (e.g. "enumerating UUIDs
	// 1-1000 to test IDOR"). Lets the judge reason about what THIS
	// specific request is for, not just the cage-wide objective.
	AgentReason string `json:"agent_reason,omitempty"`
}

type JudgeResult struct {
	Safe       bool    `json:"safe"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

type JudgeClient struct {
	endpoint            string
	confidenceThreshold float64
	apiKey              string
	httpClient          *http.Client
}

func NewJudgeClient(endpoint string, confidenceThreshold float64, apiKey string, timeout time.Duration) *JudgeClient {
	return &JudgeClient{
		endpoint:            endpoint,
		confidenceThreshold: confidenceThreshold,
		apiKey:              apiKey,
		httpClient:          &http.Client{Timeout: timeout},
	}
}

// SetTransport overrides the HTTP transport used for judge requests.
// The payload proxy uses this to set the fwmark transport so judge
// connections bypass the iptables redirect.
func (c *JudgeClient) SetTransport(t http.RoundTripper) {
	c.httpClient.Transport = t
}

// EvaluateInput bundles everything the judge LLM needs to reason about
// a request. Bundled rather than positional so adding context fields
// later doesn't churn every caller.
type EvaluateInput struct {
	CageType     string
	VulnClass    string
	AssessmentID string
	Method       string
	URL          string
	Headers      map[string]string
	Body         []byte
	Objective    string
	AgentReason  string
}

// Evaluate sends a single payload to the judge endpoint and returns a
// decision. Uses its own timeout rather than the caller's context so the
// agent's request deadline cannot cut the judge call short.
func (c *JudgeClient) Evaluate(in EvaluateInput) (PayloadDecision, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.httpClient.Timeout)
	defer cancel()

	payload, err := json.Marshal(struct {
		Payloads []JudgePayload `json:"payloads"`
	}{
		Payloads: []JudgePayload{{
			CageType:     in.CageType,
			VulnClass:    in.VulnClass,
			AssessmentID: in.AssessmentID,
			Method:       in.Method,
			URL:          in.URL,
			Headers:      in.Headers,
			Body:         string(in.Body),
			Objective:    in.Objective,
			AgentReason:  in.AgentReason,
		}},
	})
	if err != nil {
		return PayloadBlock, "judge request marshal failed", fmt.Errorf("marshaling judge request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return PayloadBlock, "judge request creation failed", fmt.Errorf("creating judge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PayloadBlock, "judge unreachable", fmt.Errorf("calling judge endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return PayloadBlock, fmt.Sprintf("judge returned status %d", resp.StatusCode), fmt.Errorf("judge endpoint returned %d", resp.StatusCode)
	}

	var judgeResp struct {
		Results []JudgeResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&judgeResp); err != nil {
		return PayloadBlock, "judge response malformed", fmt.Errorf("decoding judge response: %w", err)
	}

	if len(judgeResp.Results) != 1 {
		return PayloadBlock, "judge returned wrong result count", fmt.Errorf("expected 1 result, got %d", len(judgeResp.Results))
	}

	result := judgeResp.Results[0]
	if result.Confidence < 0 || result.Confidence > 1 {
		return PayloadBlock, "judge returned invalid confidence", fmt.Errorf("confidence %f out of [0,1] range", result.Confidence)
	}

	if result.Confidence < c.confidenceThreshold {
		return PayloadHold, result.Reason, nil
	}

	if result.Safe {
		return PayloadAllow, result.Reason, nil
	}
	return PayloadBlock, result.Reason, nil
}
