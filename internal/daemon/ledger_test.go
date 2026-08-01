package daemon

import (
	"path/filepath"
	"testing"
)

func TestLedger_RecordDedupeFeedAckPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress-ledger.json")
	l := newEgressLedger(path)

	l.record("@me/notes:0.1", ledgerBlocked, "exfil.attacker.net", "")
	l.record("@me/notes:0.1", ledgerBlocked, "exfil.attacker.net", "") // repeat: bumps count
	l.record("@me/notes:0.1", ledgerSecret, "exfil.attacker.net", "STRIPE_SECRET_KEY")

	feed := l.feed()
	if len(feed) != 1 {
		t.Fatalf("feed servers = %d, want 1", len(feed))
	}
	s := feed[0]
	if s.Ref != "@me/notes:0.1" {
		t.Fatalf("ref = %q", s.Ref)
	}
	if len(s.Events) != 2 {
		t.Fatalf("events = %d, want 2 (dedup collapsed the repeat)", len(s.Events))
	}
	var blocked *AuditEvent
	for i := range s.Events {
		if s.Events[i].Kind == ledgerBlocked {
			blocked = &s.Events[i]
		}
	}
	if blocked == nil || blocked.Count != 2 {
		t.Fatalf("blocked event count = %v, want 2", blocked)
	}
	cursor := s.Cursor

	// Ack up to the cursor with a summary; the surfaced events prune, the summary sticks.
	if err := l.ack("@me/notes:0.1", cursor, "notes tried exfil.attacker.net with your Stripe key"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	feed = l.feed()
	if len(feed[0].Events) != 0 {
		t.Fatalf("after ack, unsurfaced events = %d, want 0", len(feed[0].Events))
	}
	if feed[0].Summary == "" {
		t.Fatal("summary was not stored on ack")
	}

	// A new event after the ack shows up; the old ones stay pruned.
	l.record("@me/notes:0.1", ledgerHeld, "api.github.com", "")
	feed = l.feed()
	if len(feed[0].Events) != 1 || feed[0].Events[0].Host != "api.github.com" {
		t.Fatalf("post-ack feed = %+v, want just api.github.com", feed[0].Events)
	}

	// Reload from disk: summary and the unacked event survive.
	l2 := newEgressLedger(path)
	feed = l2.feed()
	if len(feed) != 1 || feed[0].Summary == "" || len(feed[0].Events) != 1 {
		t.Fatalf("reloaded feed = %+v, want summary + 1 event", feed)
	}
}

func TestFeedForHook_SkipsDropped(t *testing.T) {
	l := newEgressLedger(filepath.Join(t.TempDir(), "egress-ledger.json"))
	l.record("@me/notes:0.1", ledgerHeld, "api.github.com", "")
	l.record("@me/notes:0.1", ledgerDropped, "exfil.attacker.net", "")

	// The hook surfaces the held host but never a dropped one, so a denied host
	// that keeps trying does not re-alert on every caged call.
	hook := l.feedForHook()
	if len(hook) != 1 {
		t.Fatalf("hook servers = %d, want 1", len(hook))
	}
	kinds := map[string]bool{}
	for _, e := range hook[0].Events {
		kinds[e.Kind] = true
	}
	if !kinds[ledgerHeld] {
		t.Error("held event should surface to the hook")
	}
	if kinds[ledgerDropped] {
		t.Error("dropped event must not surface to the hook")
	}

	// The operator feed still shows the dropped host, so the refusal stays
	// visible in `mcpvessel audit`; it simply does not interrupt.
	feed := l.feed()
	opKinds := map[string]bool{}
	for _, e := range feed[0].Events {
		opKinds[e.Kind] = true
	}
	if !opKinds[ledgerDropped] {
		t.Error("dropped event should appear in the operator feed")
	}

	// A dropped-only window advances the hook cursor and never surfaces: a second
	// hook read after another drop returns nothing.
	l.record("@me/notes:0.1", ledgerDropped, "exfil.attacker.net", "")
	if got := l.feedForHook(); len(got) != 0 {
		t.Errorf("second hook read = %+v, want nothing (only a dropped event was added)", got)
	}
}
