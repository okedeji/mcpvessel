package config

import (
	"testing"

	"github.com/okedeji/agentcage/internal/env"
)

func TestLoad_MissingFileIsEmpty(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Providers) != 0 || c.Resources.Agents != nil {
		t.Errorf("missing config should load empty, got %+v", c)
	}
}

func TestSaveLoad_RoundTrips(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	want := &Config{
		Providers: []Endpoint{
			{Name: "openai", BaseURL: "https://api.openai.com/v1", KeyRef: "openai_key", PriceIn: 2_500_000, PriceOut: 10_000_000, Default: true},
		},
	}
	want.SetCap("@okedeji/researcher:0.1", Cap{CPUs: "2", Mem: "2g", Pids: 1024})
	want.SetCap("", Cap{CPUs: "1", Mem: "512m"})
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Providers) != 1 || got.Providers[0].KeyRef != "openai_key" || got.Providers[0].PriceIn != 2_500_000 {
		t.Errorf("providers round-trip = %+v", got.Providers)
	}
	if got.Resources.Agents["@okedeji/researcher:0.1"].Mem != "2g" {
		t.Errorf("per-agent cap round-trip = %+v", got.Resources.Agents)
	}
	if got.Resources.Defaults.CPUs != "1" {
		t.Errorf("default cap round-trip = %+v", got.Resources.Defaults)
	}
}

func TestSetProvider_ReplacesAndKeepsSingleDefault(t *testing.T) {
	c := &Config{}
	c.SetProvider(Endpoint{Name: "openai", BaseURL: "a", Default: true})
	c.SetProvider(Endpoint{Name: "anthropic", BaseURL: "b", Default: true})
	c.SetProvider(Endpoint{Name: "openai", BaseURL: "c"}) // replace openai, no longer default

	if len(c.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(c.Providers))
	}
	defaults := 0
	for _, e := range c.Providers {
		if e.Default {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("defaults = %d, want exactly 1", defaults)
	}
	if c.Providers[0].BaseURL != "c" {
		t.Errorf("openai not replaced: %+v", c.Providers[0])
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := []struct {
		name string
		c    Config
		want string
	}{
		{"two defaults", Config{Providers: []Endpoint{{Name: "a", Default: true}, {Name: "b", Default: true}}}, "one provider may be the default"},
		{"duplicate name", Config{Providers: []Endpoint{{Name: "a"}, {Name: "a"}}}, "declared twice"},
		{"negative price", Config{Providers: []Endpoint{{Name: "a", PriceIn: -1}}}, "negative pricing"},
		{"missing name", Config{Providers: []Endpoint{{BaseURL: "x"}}}, "name is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
		})
	}
}

func TestRemoveProvider(t *testing.T) {
	c := &Config{Providers: []Endpoint{{Name: "a"}, {Name: "b"}}}
	if !c.RemoveProvider("a") {
		t.Error("RemoveProvider(a) = false, want true")
	}
	if c.RemoveProvider("missing") {
		t.Error("RemoveProvider(missing) = true, want false")
	}
	if len(c.Providers) != 1 || c.Providers[0].Name != "b" {
		t.Errorf("providers after remove = %+v", c.Providers)
	}
}
