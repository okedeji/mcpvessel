package runtime

import (
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
)

func manifestWith(env map[string]string, secrets []string) *bundle.Manifest {
	return &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Env: env, Secrets: secrets}}
}

func TestInjectOperatorValues_ScopesToDeclaredNames(t *testing.T) {
	agentEnv := map[string]string{}
	m := manifestWith(map[string]string{"LOG_LEVEL": "info"}, []string{"notion_token"})
	opEnv := map[string]string{"LOG_LEVEL": "debug", "OTHER": "nope"}
	opSecrets := map[string]string{"notion_token": "ntn-secret", "elsewhere": "nope"}

	if err := injectOperatorValues(agentEnv, m, opEnv, opSecrets); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if agentEnv["LOG_LEVEL"] != "debug" {
		t.Errorf("declared ENV not overridden: %q", agentEnv["LOG_LEVEL"])
	}
	if agentEnv["notion_token"] != "ntn-secret" {
		t.Errorf("declared secret not injected: %q", agentEnv["notion_token"])
	}
	// A name the agent did not declare is never injected, even though the
	// operator pool has it.
	if _, leaked := agentEnv["OTHER"]; leaked {
		t.Error("an undeclared env key leaked into the agent")
	}
	if _, leaked := agentEnv["elsewhere"]; leaked {
		t.Error("an undeclared secret leaked into the agent")
	}
}

func TestInjectOperatorValues_MissingDeclaredSecretFailsClosed(t *testing.T) {
	m := manifestWith(nil, []string{"required_key"})
	err := injectOperatorValues(map[string]string{}, m, nil, nil)
	if err == nil {
		t.Fatal("expected a fail-closed error for a declared secret with no value")
	}
}
