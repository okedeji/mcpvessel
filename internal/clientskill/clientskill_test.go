package clientskill

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallClaudeCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	res, err := Install("claude-code")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := filepath.Join(home, ".claude", "skills", "mcpvessel", "SKILL.md")
	if res.Path != want {
		t.Fatalf("path = %q, want %q", res.Path, want)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("reading installed skill: %v", err)
	}
	wantContent, err := SkillContent("claude-code")
	if err != nil {
		t.Fatalf("reading embedded skill: %v", err)
	}
	if !bytes.Equal(got, wantContent) {
		t.Fatal("installed skill does not match the embedded content")
	}

	// Re-running overwrites in place rather than failing or duplicating.
	if _, err := Install("claude-code"); err != nil {
		t.Fatalf("second Install: %v", err)
	}
}

func TestInstallUnknownClient(t *testing.T) {
	if _, err := Install("nope"); err == nil {
		t.Fatal("expected an error for an unknown client id")
	}
}
