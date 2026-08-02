package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings is not valid JSON: %v", err)
	}
	return m
}

// hookCommands returns every command string under one hook event.
func hookCommands(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	list, _ := hooks[event].([]any)
	var cmds []string
	for _, item := range list {
		m, _ := item.(map[string]any)
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if c, ok := hm["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

func TestInstallClaudeHooks_FreshAndIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, wrote, err := installClaudeHooks()
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !wrote {
		t.Fatal("first install should report wrote=true")
	}
	if path != filepath.Join(home, ".claude", "settings.json") {
		t.Fatalf("unexpected path %s", path)
	}

	s := readSettings(t, path)
	if got := hookCommands(t, s, "PostToolUse"); len(got) != 1 || got[0] != "mcpvessel hook post-tool" {
		t.Fatalf("PostToolUse commands = %v", got)
	}
	if got := hookCommands(t, s, "SessionStart"); len(got) != 1 || got[0] != "mcpvessel hook session-start" {
		t.Fatalf("SessionStart commands = %v", got)
	}

	// Second run is a no-op: no duplicate entries, wrote=false.
	_, wrote2, err := installClaudeHooks()
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if wrote2 {
		t.Fatal("second install should report wrote=false")
	}
	s2 := readSettings(t, path)
	if got := hookCommands(t, s2, "PostToolUse"); len(got) != 1 {
		t.Fatalf("re-run duplicated PostToolUse: %v", got)
	}
}

func TestInstallClaudeHooks_PreservesExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A settings file with an unrelated top-level key and the user's own
	// PostToolUse hook already present.
	existing := `{
	  "model": "opus",
	  "hooks": {
	    "PostToolUse": [
	      {"matcher": "Bash", "hooks": [{"type": "command", "command": "my-linter"}]}
	    ]
	  }
	}`
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, wrote, err := installClaudeHooks(); err != nil || !wrote {
		t.Fatalf("install: wrote=%v err=%v", wrote, err)
	}

	s := readSettings(t, path)
	if s["model"] != "opus" {
		t.Errorf("unrelated key clobbered: model=%v", s["model"])
	}
	got := hookCommands(t, s, "PostToolUse")
	if len(got) != 2 || got[0] != "my-linter" {
		t.Errorf("did not preserve+append PostToolUse: %v", got)
	}
	if sc := hookCommands(t, s, "SessionStart"); len(sc) != 1 {
		t.Errorf("SessionStart not added: %v", sc)
	}
}

// hookMatcher returns the matcher on the entry that runs command, or "".
func hookMatcher(t *testing.T, settings map[string]any, event, command string) string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	list, _ := hooks[event].([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); c == command {
				got, _ := m["matcher"].(string)
				return got
			}
		}
	}
	return ""
}

// An install made before the matcher widened must be corrected on the next init,
// not skipped because the command is already there. Left alone, the user keeps a
// watch narrower than they believe they have, and it fails silently.
func TestInstallClaudeHooks_CorrectsADriftedMatcher(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := `{"hooks":{"PostToolUse":[{"matcher":"^mcp__mcpvessel__","hooks":[{"type":"command","command":"mcpvessel hook post-tool","timeout":5}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	path, wrote, err := installClaudeHooks()
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !wrote {
		t.Fatal("install over a stale matcher should report wrote=true")
	}
	s := readSettings(t, path)
	if got := hookMatcher(t, s, "PostToolUse", "mcpvessel hook post-tool"); got != hookMatcherPostTool {
		t.Errorf("matcher = %q, want it corrected to %q", got, hookMatcherPostTool)
	}
	if got := hookCommands(t, s, "PostToolUse"); len(got) != 1 {
		t.Errorf("PostToolUse commands = %v, want the entry fixed in place, not duplicated", got)
	}
}

// The watch must not depend on what the user named the client entry.
func TestHookMatcherPostTool_CoversAnyMcpTool(t *testing.T) {
	if hookMatcherPostTool != "^mcp__" {
		t.Fatalf("matcher = %q, want every MCP tool call watched", hookMatcherPostTool)
	}
}
