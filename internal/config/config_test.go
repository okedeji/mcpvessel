package config

import (
	"testing"
	"time"

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
		{"negative cpus", Config{Resources: Resources{Defaults: Cap{CPUs: "-1"}}}, "cpus must be a positive number"},
		{"zero cpus", Config{Resources: Resources{Defaults: Cap{CPUs: "0"}}}, "cpus must be a positive number"},
		{"garbage cpus", Config{Resources: Resources{Agents: map[string]Cap{"@o/a": {CPUs: "lots"}}}}, "cpus must be a positive number"},
		{"negative mem", Config{Resources: Resources{Agents: map[string]Cap{"@o/a": {Mem: "-2g"}}}}, "memory must be a positive size"},
		{"garbage mem", Config{Resources: Resources{Defaults: Cap{Mem: "big"}}}, "memory must be a positive size"},
		{"negative pids", Config{Resources: Resources{Defaults: Cap{Pids: -1}}}, "pids must be positive"},
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

func TestCages_EffectiveResolvesZeroToDefault(t *testing.T) {
	// Zero means "no operator value," so each knob falls to the runtime default.
	var cg Cages
	if cg.EffectiveMaxLive() != DefaultMaxLiveCages {
		t.Errorf("EffectiveMaxLive = %d, want default %d", cg.EffectiveMaxLive(), DefaultMaxLiveCages)
	}
	if cg.EffectivePrewarm() != DefaultPrewarm {
		t.Errorf("EffectivePrewarm = %d, want default %d", cg.EffectivePrewarm(), DefaultPrewarm)
	}
	if cg.EffectiveIdleTTL() != DefaultIdleTTLSeconds*time.Second {
		t.Errorf("EffectiveIdleTTL = %v, want default", cg.EffectiveIdleTTL())
	}
	// A set value wins over the default.
	set := Cages{MaxLive: 4, Prewarm: 2, IdleTTLSeconds: 60}
	if set.EffectiveMaxLive() != 4 || set.EffectivePrewarm() != 2 || set.EffectiveIdleTTL() != 60*time.Second {
		t.Errorf("set values not honored: %+v", set)
	}
}

func TestCages_ValidateRejectsNegative(t *testing.T) {
	cases := []*Config{
		{Cages: Cages{MaxLive: -1}},
		{Cages: Cages{HostMaxLive: -1}},
		{Cages: Cages{Prewarm: -1}},
		{Cages: Cages{IdleTTLSeconds: -1}},
	}
	for _, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("expected a cage policy error for %+v", c.Cages)
		}
	}
	// Zero is valid (means default), not rejected.
	if err := (&Config{Cages: Cages{}}).Validate(); err != nil {
		t.Errorf("zero cage policy rejected: %v", err)
	}
}

func TestCapMemBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1g", 1 << 30},
		{"512m", 512 << 20},
		{"2G", 2 << 30},
		{"1024k", 1024 << 10},
		{"1048576", 1048576}, // bare bytes
		{"1.5g", 1<<30 + 1<<29},
		{"", 0},
		{"big", 0},
		{"-2g", 0},
	}
	for _, tc := range cases {
		if got := (Cap{Mem: tc.in}).MemBytes(); got != tc.want {
			t.Errorf("MemBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestValidate_AcceptsValidCaps(t *testing.T) {
	c := Config{Resources: Resources{
		Defaults: Cap{CPUs: "1.5", Mem: "512m", Pids: 1024},
		Agents:   map[string]Cap{"@o/a": {CPUs: "2", Mem: "2g"}, "@o/b": {Pids: 256}},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid caps rejected: %v", err)
	}
}

func TestRemoveCap(t *testing.T) {
	c := &Config{Resources: Resources{Agents: map[string]Cap{"@o/a": {Mem: "256m"}}}}
	if !c.RemoveCap("@o/a") {
		t.Error("RemoveCap(@o/a) = false, want true")
	}
	if _, ok := c.Resources.Agents["@o/a"]; ok {
		t.Error("cap still present after RemoveCap")
	}
	if c.RemoveCap("@o/missing") {
		t.Error("RemoveCap(missing) = true, want false")
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
