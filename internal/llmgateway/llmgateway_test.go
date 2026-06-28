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

func TestControl_SetBudgetThenSpendReflectsIt(t *testing.T) {
	gw := New(Config{BudgetMicroUSD: 5000}, nil, nil)
	control := gw.Control()

	set := httptest.NewRequest(http.MethodPost, "/budget", strings.NewReader(`{"micro_usd":9000}`))
	setRec := httptest.NewRecorder()
	control.ServeHTTP(setRec, set)
	if setRec.Code != http.StatusNoContent {
		t.Fatalf("POST /budget = %d, want 204", setRec.Code)
	}

	spendRec := httptest.NewRecorder()
	control.ServeHTTP(spendRec, httptest.NewRequest(http.MethodGet, "/spend", nil))
	var snap SpendReport
	if err := json.Unmarshal(spendRec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decoding /spend: %v", err)
	}
	if snap.BudgetMicroUSD != 9000 {
		t.Errorf("budget after set = %d, want 9000", snap.BudgetMicroUSD)
	}
}

func TestControl_RejectsNegativeBudget(t *testing.T) {
	gw := New(Config{BudgetMicroUSD: 5000}, nil, nil)
	rec := httptest.NewRecorder()
	gw.Control().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/budget", strings.NewReader(`{"micro_usd":-1}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative budget = %d, want 400", rec.Code)
	}
}

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
		Agents:         map[string]AgentRoute{"tok-r": {Key: "researcher", Model: "openai/gpt-4o"}},
		BudgetMicroUSD: 5000,
	}
	gw := httptest.NewServer(New(cfg, nil, nil).Handler())
	defer gw.Close()

	resp := post(t, gw.URL+"/tok-r/chat/completions", `{"model":"placeholder","messages":[]}`)
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

	if resp := post(t, gw.URL+"/tok-r/chat/completions", `{"messages":[]}`); resp != http.StatusPaymentRequired {
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
		Agents:  map[string]AgentRoute{"tok-x": {Key: "x", Model: "anthropic/claude-3.5"}},
	}
	gw := httptest.NewServer(New(cfg, nil, nil).Handler())
	defer gw.Close()

	post(t, gw.URL+"/tok-x/chat/completions", `{"messages":[]}`)
	if seen.model != "gpt-4o-mini" {
		t.Errorf("fallback model = %q, want gpt-4o-mini", seen.model)
	}
}

func TestHandler_UnknownAgent(t *testing.T) {
	gw := httptest.NewServer(New(Config{Agents: map[string]AgentRoute{}}, nil, nil).Handler())
	defer gw.Close()
	if got := post(t, gw.URL+"/ghost/chat/completions", `{}`); got != http.StatusNotFound {
		t.Errorf("unknown agent status = %d, want 404", got)
	}
}

func TestHandler_ReportsPerAgentSpend(t *testing.T) {
	var seen providerCall
	provider := fakeProvider(t, &seen)
	defer provider.Close()

	var last SpendReport
	reports := 0
	cfg := Config{
		Endpoints: map[string]Endpoint{
			"openai": {BaseURL: provider.URL + "/v1", Key: "k", PriceIn: 2_500_000, PriceOut: 10_000_000},
		},
		Default:        "openai",
		Agents:         map[string]AgentRoute{"tok-a": {Key: "a", Model: "openai/gpt-4o"}, "tok-b": {Key: "b", Model: "openai/gpt-4o"}},
		BudgetMicroUSD: 1_000_000,
	}
	gw := httptest.NewServer(New(cfg, func(r SpendReport) { last, reports = r, reports+1 }, nil).Handler())
	defer gw.Close()

	// Routed by the opaque token, but metered by the real agent key: a twice, b once.
	post(t, gw.URL+"/tok-a/chat/completions", `{"messages":[]}`)
	post(t, gw.URL+"/tok-a/chat/completions", `{"messages":[]}`)
	post(t, gw.URL+"/tok-b/chat/completions", `{"messages":[]}`)

	if reports != 3 {
		t.Fatalf("report callbacks = %d, want 3", reports)
	}
	if last.TotalMicroUSD != 22_500 {
		t.Errorf("total = %d, want 22500", last.TotalMicroUSD)
	}
	if last.BudgetMicroUSD != 1_000_000 {
		t.Errorf("budget = %d, want 1000000", last.BudgetMicroUSD)
	}
	if got := last.Agents["a"]; got.SpentMicroUSD != 15_000 || got.Calls != 2 {
		t.Errorf("agent a = %+v, want spent 15000 calls 2", got)
	}
	if got := last.Agents["b"]; got.SpentMicroUSD != 7_500 || got.Calls != 1 {
		t.Errorf("agent b = %+v, want spent 7500 calls 1", got)
	}
}

func TestHandler_RecordsPerCallEvents(t *testing.T) {
	var seen providerCall
	provider := fakeProvider(t, &seen)
	defer provider.Close()

	var calls []CallEvent
	cfg := Config{
		Endpoints: map[string]Endpoint{
			"openai": {BaseURL: provider.URL + "/v1", Key: "k", PriceIn: 2_500_000, PriceOut: 10_000_000},
		},
		Default: "openai",
		Agents:  map[string]AgentRoute{"tok-a": {Key: "a", Model: "openai/gpt-4o"}},
	}
	record := func(e CallEvent) { calls = append(calls, e) }
	gw := httptest.NewServer(New(cfg, nil, record).Handler())
	defer gw.Close()

	post(t, gw.URL+"/tok-a/chat/completions", `{"messages":[]}`)

	if len(calls) != 1 {
		t.Fatalf("call events = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.Agent != "a" || c.Model != "gpt-4o" {
		t.Errorf("agent/model = %q/%q, want a/gpt-4o", c.Agent, c.Model)
	}
	if c.CostMicroUSD != 7_500 {
		t.Errorf("cost = %d, want 7500", c.CostMicroUSD)
	}
	if c.PromptTokens <= 0 || c.CompletionTokens <= 0 {
		t.Errorf("tokens not captured: %+v", c)
	}
	if c.EndUnixNano < c.StartUnixNano {
		t.Errorf("end before start: %+v", c)
	}
}

func TestCallLine_RoundTrips(t *testing.T) {
	want := CallEvent{Agent: "a", Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 500, CostMicroUSD: 7500, StartUnixNano: 1, EndUnixNano: 2}
	var buf strings.Builder
	WriteCallLine(&buf, want)

	got := ParseCallLines("an unrelated gateway log line\n" + buf.String())
	if len(got) != 1 || got[0] != want {
		t.Fatalf("round trip = %+v, want one %+v", got, want)
	}
}

func TestSpendLine_RoundTrips(t *testing.T) {
	want := SpendReport{
		TotalMicroUSD:  22_500,
		BudgetMicroUSD: 1_000_000,
		Agents:         map[string]AgentSpend{"a": {SpentMicroUSD: 15_000, Calls: 2}},
	}
	var buf strings.Builder
	WriteSpendLine(&buf, SpendReport{TotalMicroUSD: 1}) // an earlier, stale snapshot
	WriteSpendLine(&buf, want)

	got, ok := ParseSpendLine("some other gateway log line\n" + buf.String())
	if !ok {
		t.Fatal("ParseSpendLine found nothing")
	}
	if got.TotalMicroUSD != want.TotalMicroUSD || got.Agents["a"].Calls != 2 {
		t.Errorf("parsed last snapshot = %+v, want %+v", got, want)
	}
	if _, ok := ParseSpendLine("nothing here\n"); ok {
		t.Error("ParseSpendLine reported a snapshot from logs with none")
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
