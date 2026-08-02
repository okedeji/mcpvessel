package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/daemon"
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

// notesAlert is the demo fixture's shape: one attempt recorded as a hold and a
// block, plus the secret it carried.
func notesAlert() daemon.AuditServer {
	return daemon.AuditServer{
		Ref:     "@me/notes:0.1",
		Serving: true,
		Secrets: []string{"STRIPE_SECRET_KEY"},
		Events: []daemon.AuditEvent{
			{Kind: "held", Host: "exfil.attacker.net", Count: 1},
			{Kind: "blocked", Host: "exfil.attacker.net", Count: 1},
			{Kind: "secret", Host: "exfil.attacker.net", Detail: "STRIPE_SECRET_KEY", Count: 1},
		},
	}
}

// One attempt is one incident. The ledger records it under several kinds, and
// listing each as its own bullet reads like separate events and buries the point.
func TestCollectAlerts_CollapsesOneAttempt(t *testing.T) {
	got := collectAlerts(notesAlert().Events)
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want one per host", len(got))
	}
	if got[0].attempts != 1 {
		t.Errorf("attempts = %d, want the count not inflated by the kinds", got[0].attempts)
	}
	line := got[0].line()
	for _, want := range []string{"exfil.attacker.net", "never approved", "refused", "STRIPE_SECRET_KEY"} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q:\n%s", want, line)
		}
	}
}

// An approved host is not an alert. If it was approved, the user approved it,
// and listing it beside a theft dilutes both.
func TestCollectAlerts_IgnoresApprovals(t *testing.T) {
	events := []daemon.AuditEvent{{Kind: "approved", Host: "raw.githubusercontent.com", Count: 3}}
	if got := collectAlerts(events); len(got) != 0 {
		t.Errorf("got %v, want approvals treated as routine", got)
	}
}

// The failure this replaces: a blocked exfil was reported in a tone that let the
// model answer "hey how are you" and say nothing. The directive has to remove
// the choice.
func TestSessionStartContext_DemandsTheUserBeTold(t *testing.T) {
	got := sessionStartContext([]daemon.AuditServer{notesAlert()})
	for _, want := range []string{"MUST tell the user", "very first reply", "even if they only greeted you"} {
		if !strings.Contains(got, want) {
			t.Errorf("session-start alert missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "otherwise carry on") {
		t.Error("the blanket permission to carry on must not appear alongside an alert")
	}
	if !strings.Contains(got, "holding your STRIPE_SECRET_KEY") {
		t.Errorf("heading should name what is at stake:\n%s", got)
	}
}

// Routine traffic alone is silence: a machine where nothing went wrong injects
// the standing directive and nothing else.
func TestSessionStartContext_SilentWhenOnlyRoutine(t *testing.T) {
	quiet := daemon.AuditServer{
		Ref:     "ghcr.io/okedeji/mcpvessel-docs:0.1.2",
		Serving: true,
		Events:  []daemon.AuditEvent{{Kind: "approved", Host: "raw.githubusercontent.com", Count: 2}},
	}
	if got := sessionStartContext([]daemon.AuditServer{quiet}); got != "" {
		t.Errorf("got %q, want silence when nothing is wrong", got)
	}
}

// A judgement an earlier session reached still travels, quietly, without the
// alarm reserved for a live attempt.
func TestSessionStartContext_CarriesPriorNotes(t *testing.T) {
	prior := daemon.AuditServer{Ref: "@me/notes:0.1", Serving: true, Summary: "tried to steal a Stripe key"}
	got := sessionStartContext([]daemon.AuditServer{prior})
	if !strings.Contains(got, "tried to steal a Stripe key") {
		t.Errorf("prior note dropped:\n%s", got)
	}
	if strings.Contains(got, "MUST tell the user") {
		t.Errorf("a prior note is not a fresh alert:\n%s", got)
	}
}

func TestPostToolContext_TellsClaudeNotToBuryIt(t *testing.T) {
	got := postToolContext([]daemon.AuditServer{notesAlert()})
	for _, want := range []string{"MUST tell the user now", "exfil.attacker.net", "STRIPE_SECRET_KEY"} {
		if !strings.Contains(got, want) {
			t.Errorf("post-tool alert missing %q in:\n%s", want, got)
		}
	}
	if got := postToolContext(nil); got != "" {
		t.Errorf("got %q, want silence with nothing to report", got)
	}
}

// The poll is a latency hint keyed on the front door's name. It must never be
// read as a filter: a caged server called through any other client entry is
// still watched, just without the wait.
func TestFrontedByMcpvessel(t *testing.T) {
	if !frontedByMcpvessel("mcp__mcpvessel__me-notes_save_note") {
		t.Error("a front-door tool should get the poll")
	}
	if frontedByMcpvessel("mcp__notes__me-notes_save_note") {
		t.Error("a differently named entry should not get the poll")
	}
}
