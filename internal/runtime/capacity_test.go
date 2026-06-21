package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
)

func TestTreeBaselineMemory(t *testing.T) {
	node := func(model, egress, mem string) *agentNode {
		return &agentNode{Manifest: &bundle.Manifest{Agentfile: bundle.AgentfileSpec{
			Model: model, Egress: egress, Resources: &bundle.ResourcesSpec{Mem: mem},
		}}}
	}
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root":  node("openai/gpt-4o", "", "1g"), // reasons
			"out":   node("", "allow:example.com", "512m"),
			"plain": node("", "", "2g"), // elastic, not counted
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "out", Alias: "o"},
			{Caller: "root", Sub: "plain", Alias: "p"},
		},
	}
	gw := defaultGatewayCap.MemBytes()
	// root (1g) + MCP gateway + LLM gateway (root reasons) + egress proxy + the
	// egress cage (512m); the plain elastic cage is excluded.
	want := int64(1<<30) + gw + gw + gw + int64(512<<20)
	if got := treeBaselineMemory(tree); got != want {
		t.Errorf("treeBaselineMemory = %d, want %d", got, want)
	}

	if got := CageMemoryBytes(tree.Nodes["plain"].Manifest); got != 2<<30 {
		t.Errorf("CageMemoryBytes(declared) = %d, want 2GiB", got)
	}
	if got := CageMemoryBytes(&bundle.Manifest{}); got != defaultAgentCap.MemBytes() {
		t.Errorf("CageMemoryBytes(no resources) = %d, want the default", got)
	}
}

func TestSoloBaselineMemory(t *testing.T) {
	gw := defaultGatewayCap.MemBytes()
	const root = 1 << 30
	if got := soloBaselineMemory(root, false, false); got != root {
		t.Errorf("plain solo = %d, want just the cage %d", got, root)
	}
	if got := soloBaselineMemory(root, true, false); got != root+gw {
		t.Errorf("reasoning solo = %d, want cage + LLM gateway", got)
	}
	if got := soloBaselineMemory(root, true, true); got != root+gw+gw {
		t.Errorf("reasoning+egress solo = %d, want cage + LLM gateway + egress proxy", got)
	}
}

func TestTreeBaselineMemory_CountsDeepEgress(t *testing.T) {
	node := func(model, egress, mem string) *agentNode {
		return &agentNode{Manifest: &bundle.Manifest{Agentfile: bundle.AgentfileSpec{
			Model: model, Egress: egress, Resources: &bundle.ResourcesSpec{Mem: mem},
		}}}
	}
	// root -> mid (plain, elastic) -> deep (egress, two levels down). The deep
	// egress cage must be in the baseline even though its parent is elastic.
	tree := &runTree{
		Root: "root",
		Nodes: map[string]*agentNode{
			"root": node("", "", "1g"),
			"mid":  node("", "", "1g"),                    // elastic, excluded
			"deep": node("", "allow:example.com", "256m"), // egress at depth 2
		},
		Edges: []usesEdge{
			{Caller: "root", Sub: "mid", Alias: "m"},
			{Caller: "mid", Sub: "deep", Alias: "d"},
		},
	}
	gw := defaultGatewayCap.MemBytes()
	// root (1g) + MCP gateway + egress proxy + the deep egress cage (256m); mid is
	// elastic and excluded, and nothing reasons so there is no LLM gateway.
	want := int64(1<<30) + gw + gw + int64(256<<20)
	if got := treeBaselineMemory(tree); got != want {
		t.Errorf("treeBaselineMemory = %d, want %d (a deep egress cage must be counted)", got, want)
	}
}

func TestCompulsoryMemorySumsBaselineOnly(t *testing.T) {
	gw := defaultGatewayCap.MemBytes()
	plan := &runPlan{
		RootCap:      config.Cap{Mem: "1g"},
		LLMAgents:    map[string]string{"root": "openai/gpt-4o"}, // a reasoning run: LLM gateway present
		EgressAgents: map[string]egressAgent{},                   // no egress proxy
		Agents: []plannedAgent{
			{Node: &agentNode{Key: "warm"}, Spec: ContainerSpec{Memory: "512m"}, AlwaysWarm: true},
			{Node: &agentNode{Key: "elastic"}, Spec: ContainerSpec{Memory: "2g"}}, // elastic, excluded
		},
	}

	// root (1g) + MCP gateway + LLM gateway + one kept-warm cage (512m); the
	// elastic cage and the (absent) egress proxy are not counted.
	want := int64(1<<30) + gw + gw + int64(512<<20)
	if got := compulsoryMemory(plan); got != want {
		t.Errorf("compulsoryMemory = %d, want %d", got, want)
	}
}

func TestFitElastic(t *testing.T) {
	// Baseline: root 1g + MCP gateway (128m). One elastic cage type at 1g.
	plan := &runPlan{
		RootCap: config.Cap{Mem: "1g"},
		Agents: []plannedAgent{
			{Node: &agentNode{Key: "e"}, Spec: ContainerSpec{Memory: "1g"}},
		},
	}
	const gib = 1 << 30

	// Plenty of memory: configured cap wins. 8GiB - 1GiB reserve - ~1.1GiB
	// baseline leaves ~5.9GiB / 1GiB ~= 5 elastic, above the configured 4.
	if got, err := fitElastic(8*gib, plan, 4); err != nil || got != 4 {
		t.Errorf("fitElastic(8GiB, cap 4) = %d, %v; want 4, nil", got, err)
	}

	// Tight memory: leftover clamps the cap below the configured one. 4GiB - 1GiB
	// reserve - ~1.1GiB baseline ~= 1.9GiB / 1GiB = 1 elastic, below 4.
	if got, err := fitElastic(4*gib, plan, 4); err != nil || got != 1 {
		t.Errorf("fitElastic(4GiB, cap 4) = %d, %v; want 1, nil", got, err)
	}

	// Baseline does not fit at all: error, not a clamp.
	if _, err := fitElastic(1*gib, plan, 4); err == nil {
		t.Error("fitElastic should error when the baseline does not fit the machine")
	}
}

func TestEffectiveAvailable(t *testing.T) {
	const gib = 1 << 30
	// No setting: use the real memory.
	if got, over := effectiveAvailable(8*gib, 0); got != 8*gib || over {
		t.Errorf("unset = (%d, %v), want (8GiB, false)", got, over)
	}
	// Setting below real memory: cap to it, reserving the rest.
	if got, over := effectiveAvailable(8*gib, 4*gib); got != 4*gib || over {
		t.Errorf("cap = (%d, %v), want (4GiB, false)", got, over)
	}
	// Setting above real memory: ignored, flagged so the operator is told.
	if got, over := effectiveAvailable(4*gib, 16*gib); got != 4*gib || !over {
		t.Errorf("over-request = (%d, %v), want (4GiB, true)", got, over)
	}
}

func TestReadMemTotal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	content := "MemFree:         123456 kB\nMemTotal:       16384000 kB\nBuffers:          1000 kB\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readMemTotal(path)
	if err != nil {
		t.Fatalf("readMemTotal: %v", err)
	}
	if want := int64(16384000) * 1024; got != want {
		t.Errorf("readMemTotal = %d, want %d", got, want)
	}

	if _, err := readMemTotal(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected an error for a missing file")
	}
}
