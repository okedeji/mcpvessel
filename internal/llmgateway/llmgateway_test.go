package llmgateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeProvider records what the gateway forwarded and returns a usage block
// so the gateway has something to meter.
func fakeProvider(t *testing.T, seen *providerCall) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.Path
		seen.auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		seen.model, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":500}}`))
	}))
}

type providerCall struct{ path, auth, model string }

func TestHandler_RoutesAttachesKeyOverridesModelAndEnforcesBudget(t *testing.T) {
	var seen providerCall
	provider := fakeProvider(t, &seen)
	defer provider.Close()

	// 1000 in at $2.50/Mtok = 2500 micro-USD; 500 out at $10/Mtok = 5000;
	// 7500 per call. A 5000 budget lets the first call through, then the
	// debit puts it over for the second.
	cfg := Config{
		Endpoints: map[string]Endpoint{
			"openai": {BaseURL: provider.URL + "/v1", Key: "sk-secret", PriceIn: 2_500_000, PriceOut: 10_000_000},
		},
		Default:        "openai",
		Agents:         map[string]string{"researcher": "openai/gpt-4o"},
		BudgetMicroUSD: 5000,
	}
	gw := httptest.NewServer(Handler(cfg))
	defer gw.Close()

	resp := post(t, gw.URL+"/researcher/chat/completions", `{"model":"placeholder","messages":[]}`)
	if resp != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", resp)
	}
	if seen.path != "/v1/chat/completions" {
		t.Errorf("forwarded path = %q, want /v1/chat/completions", seen.path)
	}
	if seen.auth != "Bearer sk-secret" {
		t.Errorf("auth = %q, want Bearer sk-secret", seen.auth)
	}
	if seen.model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o (overridden from placeholder)", seen.model)
	}

	if resp := post(t, gw.URL+"/researcher/chat/completions", `{"messages":[]}`); resp != http.StatusPaymentRequired {
		t.Errorf("second call status = %d, want 402 (over budget after metering)", resp)
	}
}

func TestHandler_FallbackUsesDefaultEndpointModel(t *testing.T) {
	var seen providerCall
	provider := fakeProvider(t, &seen)
	defer provider.Close()

	// The agent wants anthropic, but only openai is configured, so it falls
	// back to the default endpoint and sends that endpoint's model.
	cfg := Config{
		Endpoints: map[string]Endpoint{
			"openai": {BaseURL: provider.URL + "/v1", Key: "k", Model: "gpt-4o-mini"},
		},
		Default: "openai",
		Agents:  map[string]string{"x": "anthropic/claude-3.5"},
	}
	gw := httptest.NewServer(Handler(cfg))
	defer gw.Close()

	post(t, gw.URL+"/x/chat/completions", `{"messages":[]}`)
	if seen.model != "gpt-4o-mini" {
		t.Errorf("fallback model = %q, want gpt-4o-mini", seen.model)
	}
}

func TestHandler_UnknownAgent(t *testing.T) {
	gw := httptest.NewServer(Handler(Config{Agents: map[string]string{}}))
	defer gw.Close()
	if got := post(t, gw.URL+"/ghost/chat/completions", `{}`); got != http.StatusNotFound {
		t.Errorf("unknown agent status = %d, want 404", got)
	}
}

func TestSecret_RedactsButMarshalsReal(t *testing.T) {
	s := Secret("sk-leak")
	if out := fmt.Sprintf("%v %s %#v", s, s, s); strings.Contains(out, "sk-leak") {
		t.Errorf("Secret leaked through formatting: %q", out)
	}
	raw, _ := json.Marshal(s)
	if string(raw) != `"sk-leak"` {
		t.Errorf("Secret JSON = %s, want the real value for the env round-trip", raw)
	}
}

func post(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
