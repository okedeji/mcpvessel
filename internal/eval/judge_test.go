package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/secrets"
)

// judgeServer returns a provider stub that replies with content and a fixed
// token usage, counting how many times it was called.
func judgeServer(t *testing.T, content string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want the bearer key", got)
		}
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{{Message: chatMessage{Role: "assistant", Content: content}}},
			Usage: tokenUsage{PromptTokens: 100, CompletionTokens: 50},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func judgeFor(t *testing.T, srv *httptest.Server) *Judgement {
	t.Helper()
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	sec, err := secrets.Load()
	if err != nil {
		t.Fatalf("secrets.Load: %v", err)
	}
	sec.Set("judge_key", "sk-test")
	cfg := &config.Config{Providers: []config.Endpoint{{
		Name: "test", BaseURL: srv.URL, KeyRef: "judge_key", Model: "m",
		PriceIn: 1_000_000, PriceOut: 2_000_000, Default: true,
	}}}
	j, err := NewJudge(cfg, sec, "")
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	return j
}

func TestJudge_HappyPath(t *testing.T) {
	srv := judgeServer(t, `{"score": 0.8, "reason": "clear and accurate"}`, nil)
	v, err := judgeFor(t, srv).Score(context.Background(), "grade clarity", "in", "out")
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if v.Score != 0.8 {
		t.Errorf("Score = %v, want 0.8", v.Score)
	}
	if v.Reason != "clear and accurate" {
		t.Errorf("Reason = %q", v.Reason)
	}
	// 100 prompt tokens at 1e6/M + 50 completion at 2e6/M = 100 + 100 micro-USD.
	if v.CostMicroUSD != 200 {
		t.Errorf("CostMicroUSD = %d, want 200", v.CostMicroUSD)
	}
}

func TestJudge_ParsesFencedJSON(t *testing.T) {
	srv := judgeServer(t, "Here is my grade:\n```json\n{\"score\": 0.6, \"reason\": \"ok\"}\n```", nil)
	v, err := judgeFor(t, srv).Score(context.Background(), "grade", "in", "out")
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if v.Score != 0.6 {
		t.Errorf("Score = %v, want 0.6 from a fenced reply", v.Score)
	}
}

func TestJudge_RetriesThenFailsClosed(t *testing.T) {
	calls := 0
	srv := judgeServer(t, "I cannot produce a score", &calls)
	_, err := judgeFor(t, srv).Score(context.Background(), "grade", "in", "out")
	if err == nil || !strings.Contains(err.Error(), "unscorable") {
		t.Fatalf("err = %v, want an unscorable-reply failure", err)
	}
	if calls != 2 {
		t.Errorf("provider called %d times, want one retry (2)", calls)
	}
}

func TestNewJudge_Errors(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	sec, _ := secrets.Load()
	sec.Set("k", "sk-test")

	withDefault := &config.Config{Providers: []config.Endpoint{{Name: "openai", BaseURL: "http://x", KeyRef: "k", Model: "m", Default: true}}}
	noKey := &config.Config{Providers: []config.Endpoint{{Name: "openai", BaseURL: "http://x", KeyRef: "missing", Model: "m", Default: true}}}
	noDefault := &config.Config{Providers: []config.Endpoint{{Name: "openai", BaseURL: "http://x", KeyRef: "k", Model: "m"}}}

	tests := []struct {
		name     string
		cfg      *config.Config
		override string
		wantSub  string
	}{
		{"unknown override provider", withDefault, "ghost/model", "not configured"},
		{"malformed override", withDefault, "no-slash", "provider/model form"},
		{"no default", noDefault, "", "no default LLM provider"},
		{"missing key", noKey, "", "not in the secret store"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewJudge(tc.cfg, sec, tc.override)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestJudgement_RedactsKey(t *testing.T) {
	j := &Judgement{model: "m", apiKey: "sk-supersecret"}
	rendered := []string{
		fmt.Sprintf("%v", j),
		fmt.Sprintf("%#v", j),
		j.String(),
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	rendered = append(rendered, string(b))
	for _, s := range rendered {
		if strings.Contains(s, "sk-supersecret") {
			t.Errorf("rendered judgement leaks the key: %q", s)
		}
	}
}
