package runtime

import (
	"testing"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/secrets"
)

func TestBuildLLMConfig_ResolvesKeysDefaultAndBudget(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())

	store, _ := secrets.Load()
	store.Set("openai_key", "sk-real")
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	cfg.SetProvider(config.Endpoint{Name: "openai", BaseURL: "https://api.openai.com/v1", KeyRef: "openai_key", Model: "gpt-4o", PriceIn: 2_500_000, Default: true})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	llmCfg, err := buildLLMConfig(map[string]string{"root": "openai/gpt-4o"}, map[string]string{"root": "tok-root"}, 5_000_000)
	if err != nil {
		t.Fatalf("buildLLMConfig: %v", err)
	}
	ep := llmCfg.Endpoints["openai"]
	if string(ep.Key) != "sk-real" {
		t.Errorf("key not resolved from the secret store: %q", string(ep.Key))
	}
	if llmCfg.Default != "openai" || llmCfg.BudgetMicroUSD != 5_000_000 {
		t.Errorf("default/budget = %q/%d", llmCfg.Default, llmCfg.BudgetMicroUSD)
	}
	// Keyed by the agent's token, carrying its real key for metering.
	if got := llmCfg.Agents["tok-root"]; got.Key != "root" || got.Model != "openai/gpt-4o" {
		t.Errorf("agents = %v", llmCfg.Agents)
	}
}

func TestBuildLLMConfig_MissingSecretFailsClosed(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	cfg, _ := config.Load()
	cfg.SetProvider(config.Endpoint{Name: "openai", BaseURL: "x", KeyRef: "absent"})
	_ = cfg.Save()
	if _, err := buildLLMConfig(map[string]string{"root": "openai/x"}, map[string]string{"root": "t"}, 0); err == nil {
		t.Error("expected an error when an endpoint's secret is not set")
	}
}

func TestBuildLLMConfig_NoProvidersFailsClosed(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	if _, err := buildLLMConfig(map[string]string{"root": "openai/x"}, map[string]string{"root": "t"}, 0); err == nil {
		t.Error("expected an error when a reasoning agent has no provider configured")
	}
}
