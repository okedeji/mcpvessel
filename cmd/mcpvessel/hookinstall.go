package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Claude Code's watch is two hooks in the user's settings: a SessionStart hook
// that reports what caged servers did while away, and a PostToolUse hook scoped
// to the caged front door that surfaces new egress right after a tool call. Both
// exec `mcpvessel hook ...`. The command is the bare binary name (PATH-resolved,
// like the skill's own commands), so an upgrade that swaps the binary in place
// keeps the same settings entry and re-running init stays idempotent.
// hookMatcherPostTool covers every MCP tool call, not just the caged front
// door's. The door's client-side name is chosen at `claude mcp add` time and the
// user can rename it or register a caged server separately; scoping to one name
// meant watching stopped silently the moment they did. The hook itself is cheap
// on a foreign tool: one short daemon read, no poll.
const (
	hookMatcherPostTool     = "^mcp__"
	hookMatcherSessionStart = "startup|clear"
	hookTimeoutSeconds      = 5
)

// claudeSettingsPath is the user-scope settings file Claude Code reads on every
// session, so a hook here applies globally, not per project.
func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// installClaudeHooks merges mcpvessel's PostToolUse and SessionStart hooks into
// ~/.claude/settings.json, leaving every other key and any existing hooks the
// user has untouched. It returns whether it wrote (false when both entries were
// already present) so init can report accurately.
func installClaudeHooks() (path string, wrote bool, err error) {
	path, err = claudeSettingsPath()
	if err != nil {
		return "", false, err
	}

	root := map[string]any{}
	if data, rerr := os.ReadFile(path); rerr == nil {
		if len(data) > 0 {
			if uerr := json.Unmarshal(data, &root); uerr != nil {
				return path, false, fmt.Errorf("parsing %s (leave it valid and re-run): %w", path, uerr)
			}
		}
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return path, false, rerr
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}

	changed := ensureHookEntry(hooks, "PostToolUse", hookMatcherPostTool, "mcpvessel hook post-tool")
	changed = ensureHookEntry(hooks, "SessionStart", hookMatcherSessionStart, "mcpvessel hook session-start") || changed
	if !changed {
		return path, false, nil
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return path, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, false, err
	}
	// Write via a temp file and rename, so a crash mid-write cannot truncate the
	// user's whole Claude Code settings (their allowlists, other hooks, config).
	tmp := path + ".mcpvessel.tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return path, false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return path, false, err
	}
	return path, true, nil
}

// ensureHookEntry appends one command hook under event with the given matcher,
// unless an entry already runs that exact command. It preserves the user's other
// entries for the event. Reports whether it changed anything.
//
// An entry that already runs the command still has its matcher corrected when it
// has drifted from the current one. Without that, an install made before a
// matcher change keeps the old scope forever: re-running init would find the
// command, call it done, and leave the user with a narrower watch than they
// think they have.
func ensureHookEntry(hooks map[string]any, event, matcher, command string) bool {
	list, _ := hooks[event].([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if got, _ := hm["command"].(string); got != command {
				continue
			}
			if got, _ := m["matcher"].(string); got != matcher {
				m["matcher"] = matcher
				return true
			}
			return false
		}
	}
	hooks[event] = append(list, map[string]any{
		"matcher": matcher,
		"hooks": []any{
			map[string]any{"type": "command", "command": command, "timeout": hookTimeoutSeconds},
		},
	})
	return true
}
