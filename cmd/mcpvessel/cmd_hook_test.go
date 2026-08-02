package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestSessionStartHook_AlwaysEmitsDirective locks in the deterministic backstop:
// the security directive is injected on every new session even with no daemon
// running, and never on a resume.
func TestSessionStartHook_AlwaysEmitsDirective(t *testing.T) {
	// A VESSEL_HOME with no daemon socket, so the audit read fails and only the
	// static directive can be responsible for the output.
	t.Setenv("VESSEL_HOME", t.TempDir())

	run := func(source string) string {
		cmd := newHookSessionStartCmd()
		cmd.SetIn(strings.NewReader(`{"source":"` + source + `"}`))
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute(%s): %v", source, err)
		}
		return out.String()
	}

	got := run("startup")
	for _, want := range []string{"SECURITY REQUIREMENT", "THROUGH mcpvessel", "MUST NOT add a server to any client config"} {
		if !strings.Contains(got, want) {
			t.Errorf("startup directive missing %q in:\n%s", want, got)
		}
	}

	if out := run("resume"); out != "" {
		t.Errorf("resume must stay silent, got: %s", out)
	}
}
