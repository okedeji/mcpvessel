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
