package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/secrets"
)

// judgeTimeout bounds one grading call so a wedged provider fails the case,
// not the whole suite. Generous: a large output is real work to grade.
const judgeTimeout = 90 * time.Second

// judgeInstruction is appended to the author's rubric. Strict on purpose: an
// unparseable reply fails the case closed, never a silent pass.
const judgeInstruction = `Reply with only this JSON and nothing else: {"score": <number between 0.0 and 1.0>, "reason": "<one sentence>"}`

// Judgement grades output against a rubric via an operator-configured
// provider. It runs in the trusted CLI process, never in a cage, because it
// holds a provider key that must never reach agent code; the redacting
// methods below keep that key out of any log line.
type Judgement struct {
	baseURL  string
	model    string
	apiKey   string
	priceIn  int64
	priceOut int64
	client   *http.Client
}

var _ scorer = (*Judgement)(nil)

func (j Judgement) String() string               { return fmt.Sprintf("eval.Judgement{model:%q}", j.model) }
func (j Judgement) GoString() string             { return j.String() }
func (j Judgement) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// NewJudge resolves the judge's model and provider key. override
// ("provider/model") must name a configured provider: an operator's typo
// should stop them, not be papered over with the default. Empty override uses
// the default provider. A missing default or key fails closed before any
// case runs.
func NewJudge(cfg *config.Config, sec *secrets.Store, override string) (*Judgement, error) {
	ep, model, err := resolveJudgeEndpoint(cfg, override)
	if err != nil {
		return nil, err
	}
	if ep.KeyRef == "" {
		return nil, fmt.Errorf("judge provider %q has no key_ref configured", ep.Name)
	}
	key, ok := sec.Get(ep.KeyRef)
	if !ok {
		return nil, fmt.Errorf("judge provider %q key %q is not in the secret store; run 'agentcage secrets set %s'", ep.Name, ep.KeyRef, ep.KeyRef)
	}
	return &Judgement{
		baseURL:  strings.TrimRight(ep.BaseURL, "/"),
		model:    model,
		apiKey:   key,
		priceIn:  ep.PriceIn,
		priceOut: ep.PriceOut,
		client:   &http.Client{},
	}, nil
}

func resolveJudgeEndpoint(cfg *config.Config, override string) (config.Endpoint, string, error) {
	if override != "" {
		provider, model, ok := strings.Cut(override, "/")
		if !ok || provider == "" || model == "" {
			return config.Endpoint{}, "", fmt.Errorf("judge model %q must be in provider/model form", override)
		}
		for _, e := range cfg.Providers {
			if e.Name == provider {
				return e, model, nil
			}
		}
		return config.Endpoint{}, "", fmt.Errorf("judge model %q: provider %q is not configured; run 'agentcage config provider set %s ...' or pick a configured provider", override, provider, provider)
	}
	for _, e := range cfg.Providers {
		if e.Default {
			if e.Model == "" {
				return config.Endpoint{}, "", fmt.Errorf("default provider %q has no model to judge with; pass --judge-model provider/model", e.Name)
			}
			return e, e.Model, nil
		}
	}
	return config.Endpoint{}, "", fmt.Errorf("no default LLM provider to judge with; set one with 'agentcage config provider set ... --default' or pass --judge-model provider/model")
}

// Score grades output against rubric. It retries once on an unparseable
// reply, then fails closed: a judge that cannot produce a number is never
// read as a pass.
func (j *Judgement) Score(ctx context.Context, rubric, input, output string) (Verdict, error) {
	ctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()

	body := chatRequest{
		Model:       j.model,
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: rubric + "\n\n" + judgeInstruction},
			{Role: "user", Content: fmt.Sprintf("Input:\n%s\n\nOutput:\n%s", input, output)},
		},
	}

	var totalCost int64
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		content, usage, err := j.chat(ctx, body)
		if err != nil {
			return Verdict{}, err
		}
		totalCost += costMicroUSD(usage, j.priceIn, j.priceOut)
		verdict, perr := parseVerdict(content)
		if perr == nil {
			verdict.CostMicroUSD = totalCost
			return verdict, nil
		}
		lastErr = perr
	}
	return Verdict{}, fmt.Errorf("judge returned an unscorable reply: %w", lastErr)
}

func (j *Judgement) chat(ctx context.Context, body chatRequest) (string, tokenUsage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", tokenUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", tokenUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.apiKey)

	resp, err := j.client.Do(req)
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("calling judge provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", tokenUsage{}, fmt.Errorf("judge provider returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", tokenUsage{}, fmt.Errorf("decoding judge response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", tokenUsage{}, fmt.Errorf("judge response carried no choices")
	}
	return cr.Choices[0].Message.Content, cr.Usage, nil
}

// parseVerdict decodes the first brace-delimited object in the reply,
// tolerating a stray code fence or lead-in sentence around the asked-for
// bare JSON.
func parseVerdict(content string) (Verdict, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return Verdict{}, fmt.Errorf("no JSON object in reply %q", truncate(content, 120))
	}
	var raw struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(content[start:end+1]), &raw); err != nil {
		return Verdict{}, fmt.Errorf("reply %q is not a score object: %w", truncate(content, 120), err)
	}
	score := raw.Score
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return Verdict{Score: score, Reason: raw.Reason}, nil
}

// costMicroUSD mirrors the LLM gateway's meter: prices are micro-USD per
// million tokens, integer math, no float drift.
func costMicroUSD(u tokenUsage, priceIn, priceOut int64) int64 {
	return u.PromptTokens*priceIn/1_000_000 + u.CompletionTokens*priceOut/1_000_000
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage tokenUsage `json:"usage"`
}

type tokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}
