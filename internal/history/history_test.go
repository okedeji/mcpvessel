package history

import (
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := open(t)
	started := time.Now().Truncate(time.Second)
	want := Record{RunID: "echo-abc123", Ref: "@me/echo:1", Status: StatusRunning, StartedAt: started}
	if err := s.Put(want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, found, err := s.Get("echo-abc123")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got.Ref != want.Ref || got.Status != want.Status || !got.StartedAt.Equal(started) {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestGetMissing(t *testing.T) {
	s := open(t)
	if _, found, err := s.Get("nope"); found || err != nil {
		t.Fatalf("Get missing: found=%v err=%v", found, err)
	}
}

func TestPutOverwritesAndListIsNewestFirst(t *testing.T) {
	s := open(t)
	old := time.Now().Add(-time.Hour)
	recent := time.Now()
	if err := s.Put(Record{RunID: "a", Status: StatusRunning, StartedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Record{RunID: "b", Status: StatusRunning, StartedAt: recent}); err != nil {
		t.Fatal(err)
	}
	// Terminal write for "a" must overwrite its running entry, not add one.
	if err := s.Put(Record{RunID: "a", Status: StatusSucceeded, StartedAt: old, CostMicroUSD: 12_000}); err != nil {
		t.Fatal(err)
	}

	recs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].RunID != "b" {
		t.Errorf("newest first: want b, got %s", recs[0].RunID)
	}
	if recs[1].RunID != "a" || recs[1].Status != StatusSucceeded || recs[1].CostMicroUSD != 12_000 {
		t.Errorf("overwrite lost: got %+v", recs[1])
	}
}

func TestReconcileRunningOnlyTouchesRunning(t *testing.T) {
	s := open(t)
	now := time.Now()
	for _, r := range []Record{
		{RunID: "live", Status: StatusRunning, StartedAt: now},
		{RunID: "done", Status: StatusSucceeded, StartedAt: now, EndedAt: now},
	} {
		if err := s.Put(r); err != nil {
			t.Fatal(err)
		}
	}

	at := now.Add(time.Minute)
	n, err := s.ReconcileRunning(at)
	if err != nil {
		t.Fatalf("ReconcileRunning: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 reconciled, got %d", n)
	}

	live, _, _ := s.Get("live")
	if live.Status != StatusCrashed || !live.EndedAt.Equal(at) {
		t.Errorf("running run not reconciled to crashed: %+v", live)
	}
	done, _, _ := s.Get("done")
	if done.Status != StatusSucceeded {
		t.Errorf("finished run must be left alone: %+v", done)
	}
}

func TestReconcileRunningIdempotent(t *testing.T) {
	s := open(t)
	now := time.Now()
	if err := s.Put(Record{RunID: "x", Status: StatusRunning, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileRunning(now); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReconcileRunning(now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReconcileRunning: %v", err)
	}
	if n != 0 {
		t.Fatalf("second reconcile must touch nothing, reconciled %d", n)
	}
}
