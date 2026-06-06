package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	maxRetries     = 3
	baseRetryDelay = 500 * time.Millisecond

	// Number of consecutive auth failures before dispatching an alert.
	// Single 401s can happen during key rotation; sustained failures
	// indicate a real problem.
	authFailureAlertThreshold = 3
)

// AlertNotifier dispatches gateway operational alerts. Satisfied by
// alert.Dispatcher without gateway importing the alert package.
type AlertNotifier interface {
	Notify(ctx context.Context, source, category, description, cageID, assessmentID string, priority int, details map[string]any)
}

// EndpointFunc returns the current LLM endpoint. Called on every
// request so config set llm.endpoint takes effect immediately.
type EndpointFunc func() string

// TimeoutFunc returns the per-request timeout. Called on every
// request so config set llm.timeout takes effect without restarting
// the orchestrator. Falls back to 30s when the resolver returns a
// non-positive value.
type TimeoutFunc func() time.Duration

type Client struct {
	endpointFn EndpointFunc
	apiKey     string
	httpClient *http.Client
	meter      *TokenMeter
	budget     *BudgetEnforcer
	alerter    AlertNotifier
	timeoutFn  TimeoutFunc

	authFailMu    sync.Mutex
	authFailures  int
	authAlertSent bool
}

func NewClient(endpointFn EndpointFunc, apiKey string, timeoutFn TimeoutFunc, meter *TokenMeter, budget *BudgetEnforcer, alerter AlertNotifier) *Client {
	// Tuned for high-concurrency single-endpoint workload: thousands of cages
	// all talking to one gateway. Default MaxIdleConnsPerHost=2 would force
	// most requests to redo TCP+TLS handshakes. The http.Client has no
	// Timeout; per-request deadlines come from the request context, which
	// the retry loop derives from the live timeoutFn value.
	transport := otelhttp.NewTransport(&http.Transport{
		MaxIdleConns:        500,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	})
	return &Client{
		endpointFn: endpointFn,
		apiKey:     apiKey,
		httpClient: &http.Client{Transport: transport},
		meter:      meter,
		budget:     budget,
		alerter:    alerter,
		timeoutFn:  timeoutFn,
	}
}

func (c *Client) requestTimeout() time.Duration {
	if c.timeoutFn == nil {
		return 30 * time.Second
	}
	t := c.timeoutFn()
	if t <= 0 {
		return 30 * time.Second
	}
	return t
}

func (c *Client) ChatCompletion(ctx context.Context, cageID, assessmentID string, tokenBudget int64, req LLMRequest) (*LLMResponse, error) {
	if err := c.budget.Check(cageID, tokenBudget); err != nil {
		return nil, fmt.Errorf("checking budget for cage %s: %w", cageID, err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling LLM request: %w", err)
	}

	respBody, err := c.doWithRetry(ctx, body)
	if err != nil {
		return nil, err
	}

	var resp LLMResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshaling LLM response: %w", err)
	}

	if resp.Usage.TotalTokens == 0 {
		return nil, ErrNoUsageData
	}

	c.meter.Record(cageID, assessmentID, resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

	return &resp, nil
}

func (c *Client) doWithRetry(ctx context.Context, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseRetryDelay * time.Duration(1<<(attempt-1)) // 500ms, 1s, 2s
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
		httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpointFn(), bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("creating HTTP request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("x-api-key", c.apiKey)
		}

		httpResp, err := c.httpClient.Do(httpReq)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("sending request to LLM gateway (attempt %d): %w", attempt+1, err)
			continue
		}

		respBody, readErr := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		cancel()

		if readErr != nil {
			lastErr = fmt.Errorf("reading LLM gateway response (attempt %d): %w", attempt+1, readErr)
			continue
		}

		// Retry on 5xx (transient gateway/provider failures), give up on 4xx (client errors).
		// Body excerpt is included on non-2xx so the actual provider
		// error (e.g. "You exceeded your current quota") shows up in
		// orchestrator logs instead of just the bare status code.
		if httpResp.StatusCode >= 500 {
			lastErr = fmt.Errorf("LLM gateway returned HTTP %d (attempt %d): %s", httpResp.StatusCode, attempt+1, bodyExcerpt(respBody))
			continue
		}
		if httpResp.StatusCode == http.StatusUnauthorized || httpResp.StatusCode == http.StatusForbidden {
			c.recordAuthFailure(ctx, httpResp.StatusCode)
			// Auth-error bodies sometimes echo the rejected key. Suppress
			// the body to keep partial credentials out of logs.
			return nil, fmt.Errorf("LLM gateway returned HTTP %d (auth)", httpResp.StatusCode)
		}
		if httpResp.StatusCode >= 400 {
			return nil, fmt.Errorf("LLM gateway returned HTTP %d: %s", httpResp.StatusCode, bodyExcerpt(respBody))
		}

		c.resetAuthFailures()
		return respBody, nil
	}
	return nil, fmt.Errorf("LLM gateway failed after %d attempts: %w", maxRetries, lastErr)
}

// bodyExcerpt returns a single-line view of a response body suitable
// for embedding in an error message. Caps at 2KB so a giant provider
// response can't blow up the orchestrator log line.
func bodyExcerpt(b []byte) string {
	const cap = 2048
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "<empty body>"
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > cap {
		return s[:cap] + "…"
	}
	return s
}

func (c *Client) recordAuthFailure(ctx context.Context, status int) {
	c.authFailMu.Lock()
	c.authFailures++
	count := c.authFailures
	shouldAlert := count >= authFailureAlertThreshold && !c.authAlertSent
	if shouldAlert {
		c.authAlertSent = true
	}
	c.authFailMu.Unlock()

	if shouldAlert && c.alerter != nil {
		c.alerter.Notify(ctx, "behavioral", "gateway_auth_failed",
			fmt.Sprintf("LLM gateway returned HTTP %d for %d consecutive requests, check API key", status, count),
			"", "", 4, map[string]any{"status": status, "consecutive_failures": count})
	}
}

func (c *Client) resetAuthFailures() {
	c.authFailMu.Lock()
	c.authFailures = 0
	c.authAlertSent = false
	c.authFailMu.Unlock()
}
