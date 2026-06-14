package runtime

import (
	"testing"

	"github.com/okedeji/agentcage/internal/config"
)

func node(t *testing.T, ref string) *agentNode {
	t.Helper()
	return &agentNode{Ref: mustParseRef(t, ref)}
}

func TestEffectiveModel_OverrideWinsByName(t *testing.T) {
	n := node(t, "@okedeji/researcher:0.1")
	models := map[string]string{"@okedeji/researcher": "openai/gpt-4o-mini"}
	if got := effectiveModel("anthropic/claude-3.5", n, models); got != "openai/gpt-4o-mini" {
		t.Errorf("override = %q, want openai/gpt-4o-mini", got)
	}
	// No override -> advisory.
	if got := effectiveModel("anthropic/claude-3.5", n, nil); got != "anthropic/claude-3.5" {
		t.Errorf("advisory = %q", got)
	}
}

func TestAgentCap_PerFieldFallback(t *testing.T) {
	n := node(t, "@org/web:1.0")
	res := config.Resources{
		Defaults: config.Cap{CPUs: "1", Mem: "512m", Pids: 256},
		Agents:   map[string]config.Cap{"@org/web": {Mem: "4g"}}, // only mem overridden
	}
	cap := agentCap(n, res)
	if cap.Mem != "4g" {
		t.Errorf("mem = %q, want 4g (per-agent override)", cap.Mem)
	}
	if cap.CPUs != "1" {
		t.Errorf("cpus = %q, want 1 (operator default)", cap.CPUs)
	}
	if cap.Pids != 256 {
		t.Errorf("pids = %d, want 256 (operator default)", cap.Pids)
	}
}

func TestAgentCap_RuntimeDefaultWhenUnset(t *testing.T) {
	cap := agentCap(node(t, "@org/x:1.0"), config.Resources{})
	if cap.Mem != defaultAgentCap.Mem || cap.Pids != defaultAgentCap.Pids {
		t.Errorf("cap = %+v, want runtime default %+v", cap, defaultAgentCap)
	}
}
