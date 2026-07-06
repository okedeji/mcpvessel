package config

import (
	"testing"

	"github.com/okedeji/agentcage/internal/env"
)

const knob = "AGENTCAGE_TEST_KNOB"

func storeEnv(t *testing.T, name, value string) {
	t.Helper()
	t.Setenv(env.Home, t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.SetEnv(name, value)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestLookupEnv_EnvWinsOverConfig(t *testing.T) {
	storeEnv(t, knob, "from-config")
	t.Setenv(knob, "from-shell")
	if got := LookupEnv(knob); got != "from-shell" {
		t.Errorf("LookupEnv = %q, want the shell value to win", got)
	}
}

func TestLookupEnv_ConfigWhenEnvUnset(t *testing.T) {
	storeEnv(t, knob, "from-config")
	// knob is not exported, so the stored value applies.
	if got := LookupEnv(knob); got != "from-config" {
		t.Errorf("LookupEnv = %q, want the stored config value", got)
	}
}

func TestLookupEnv_BlankEnvFallsThrough(t *testing.T) {
	storeEnv(t, knob, "from-config")
	t.Setenv(knob, "   ")
	if got := LookupEnv(knob); got != "from-config" {
		t.Errorf("LookupEnv = %q, want a blank env to count as unset and fall through", got)
	}
}

func TestLookupEnvOr_Default(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	if got := LookupEnvOr(knob, "the-default"); got != "the-default" {
		t.Errorf("LookupEnvOr = %q, want the default when nothing is set", got)
	}
}

func TestSetRemoveEnv(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.SetEnv(knob, "v")
	if c.Env[knob] != "v" {
		t.Fatalf("SetEnv did not store the value: %v", c.Env)
	}
	if !c.RemoveEnv(knob) {
		t.Error("RemoveEnv reported not-present for a set knob")
	}
	if _, ok := c.Env[knob]; ok {
		t.Error("RemoveEnv left the knob in place")
	}
	if c.RemoveEnv(knob) {
		t.Error("RemoveEnv reported present for an absent knob")
	}
}
