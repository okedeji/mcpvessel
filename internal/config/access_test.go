package config

import (
	"strings"
	"testing"
)

func TestEgressPolicy_ForFallbackOrder(t *testing.T) {
	c := &Config{}
	c.SetEgress("", []string{"default.example"})
	c.SetEgress("@me/github", []string{"api.github.com"})
	c.SetEgress("@me/github:0.1", []string{"objects.githubusercontent.com"})

	// A versioned run gets the version list, the name list, and the defaults.
	got := c.Egress.For("@me/github:0.1", "@me/github")
	want := "api.github.com,default.example,objects.githubusercontent.com" // deduped + sorted
	if strings.Join(got, ",") != want {
		t.Errorf("For(version) = %v, want %v", got, want)
	}

	// An untagged/local run gets only the defaults.
	if got := c.Egress.For("", ""); strings.Join(got, ",") != "default.example" {
		t.Errorf("For(local) = %v, want default only", got)
	}
}

func TestConfig_AddAndRemoveEgress(t *testing.T) {
	c := &Config{}
	c.AddEgress("@me/github:0.1", "api.github.com")
	c.AddEgress("@me/github:0.1", "api.github.com", "objects.githubusercontent.com") // dedups
	if got := c.Egress.Agents["@me/github:0.1"]; strings.Join(got, ",") != "api.github.com,objects.githubusercontent.com" {
		t.Fatalf("AddEgress = %v", got)
	}
	if !c.RemoveEgressHost("@me/github:0.1", "api.github.com") {
		t.Fatal("RemoveEgressHost reported absent")
	}
	if got := c.Egress.Agents["@me/github:0.1"]; strings.Join(got, ",") != "objects.githubusercontent.com" {
		t.Errorf("after host removal = %v", got)
	}
	// Removing the last host drops the entry entirely.
	c.RemoveEgressHost("@me/github:0.1", "objects.githubusercontent.com")
	if _, ok := c.Egress.Agents["@me/github:0.1"]; ok {
		t.Error("emptied entry should be deleted")
	}
}

func TestSecretPolicy_ForAndValidate(t *testing.T) {
	c := &Config{}
	c.SetSecretBinding("", []string{"OTEL_TOKEN"})
	c.SetSecretBinding("@me/github:0.1", []string{"GITHUB_TOKEN"})
	got := c.Secrets.For("@me/github:0.1", "@me/github")
	if strings.Join(got, ",") != "GITHUB_TOKEN,OTEL_TOKEN" {
		t.Errorf("secret For = %v", got)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	// A hand-edited empty-list entry is rejected.
	c.Secrets.Agents["@bad/x:1"] = nil
	if err := c.Validate(); err == nil {
		t.Error("empty binding list should fail validation")
	}
}
