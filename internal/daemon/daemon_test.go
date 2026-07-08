package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/history"
	"github.com/okedeji/agentcage/internal/identity"
)

// shortSocket returns a socket path under a short /tmp dir, not t.TempDir():
// macOS caps a socket path at 104 bytes, and a long test name under
// /var/folders pushes t.TempDir()'s path over it.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ac")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "agentcage.sock")
}

func TestCheckSocketPathLen(t *testing.T) {
	if err := checkSocketPathLen("/tmp/ac/agentcage.sock"); err != nil {
		t.Errorf("a short path should pass: %v", err)
	}
	long := "/var/folders/hz/" + strings.Repeat("x", maxSocketPathLen) + "/agentcage.sock"
	err := checkSocketPathLen(long)
	if err == nil {
		t.Fatal("an over-limit path must be rejected with a clear error")
	}
	if !strings.Contains(err.Error(), "AGENTCAGE_HOME") {
		t.Errorf("error should point the operator at the fix: %v", err)
	}
}

func TestFront_ClosesWhenLastRunStops(t *testing.T) {
	d := New()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.NewServeMux()}
	d.addFront(srv, []string{"run-1", "run-2"})
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// One run still live: the door stays open.
	d.releaseFrontFor("run-1")
	select {
	case err := <-serveErr:
		t.Fatalf("front closed with a run still live: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Last run stops: the door closes and frees its port.
	d.releaseFrontFor("run-2")
	select {
	case err := <-serveErr:
		if err != http.ErrServerClosed {
			t.Errorf("Serve returned %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("front did not close after its last run stopped")
	}
}

func TestRegistry_HoldTake(t *testing.T) {
	d := New()
	d.hold(RunInfo{ID: "researcher-abc"}, nil)
	if _, ok := d.take("researcher-abc"); !ok {
		t.Error("take of a held run should report true")
	}
	if _, ok := d.take("researcher-abc"); ok {
		t.Error("take of an already-taken run should report false")
	}
}

func TestListRuns_MergesHistoryAndLive(t *testing.T) {
	d := New()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d.hist = store

	now := time.Now()
	if err := store.Put(history.Record{RunID: "done-1", Ref: "@me/echo:1", Status: history.StatusSucceeded, StartedAt: now.Add(-time.Minute), EndedAt: now, CostMicroUSD: 12_000}); err != nil {
		t.Fatal(err)
	}
	d.hold(RunInfo{ID: "live-1", Ref: "@me/researcher:1", Status: "running", StartedAt: now}, nil)
	if err := store.Put(history.Record{RunID: "live-1", Ref: "@me/researcher:1", Status: history.StatusRunning, StartedAt: now}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	d.handleListRuns(rec, httptest.NewRequest(http.MethodGet, "/runs", nil))

	var body struct {
		Runs []RunInfo `json:"runs"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %+v", len(body.Runs), body.Runs)
	}
	byID := map[string]RunInfo{}
	for _, r := range body.Runs {
		byID[r.ID] = r
	}
	if got := byID["done-1"]; got.Status != history.StatusSucceeded || got.CostMicroUSD != 12_000 {
		t.Errorf("finished run not surfaced from history: %+v", got)
	}
	if got := byID["live-1"]; got.Status != "running" || !got.EndedAt.IsZero() {
		t.Errorf("live run should win over its stored running record: %+v", got)
	}
}

func TestServe_SocketRoundTrip(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	socket := shortSocket(t)
	d := New()
	d.hold(RunInfo{
		ID:        "researcher-abc",
		Ref:       "@me/researcher:0.1",
		Status:    "running",
		StartedAt: time.Now(),
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, d, socket) }()

	c := Dial(socket)
	ver := waitForDaemon(t, c)
	if ver != identity.Version {
		t.Errorf("Version = %q, want %q", ver, identity.Version)
	}

	runs, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "researcher-abc" {
		t.Errorf("ListRuns = %+v, want one run researcher-abc", runs)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve returned %v on clean shutdown", err)
		}
	case <-time.After(shutdownTimeout + 2*time.Second):
		t.Fatal("Serve did not shut down after context cancel")
	}
}

// Serve runs against a background context here, so only the /shutdown request
// can stop it.
func TestShutdown_StopsServe(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	socket := shortSocket(t)
	d := New()
	errc := make(chan error, 1)
	go func() { errc <- Serve(context.Background(), d, socket) }()

	c := Dial(socket)
	waitForDaemon(t, c)

	// The ack can race the socket closing; Serve returning is the proof.
	_ = c.Shutdown(context.Background())
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve returned %v after a shutdown request", err)
		}
	case <-time.After(shutdownTimeout + 2*time.Second):
		t.Fatal("Serve did not stop after /shutdown")
	}
}

func TestServe_RefusesSecondListener(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	socket := shortSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, New(), socket) }()
	waitForDaemon(t, Dial(socket))

	// Bounded so a regression (binding anyway) fails instead of blocking on a
	// healthy Serve.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := Serve(ctx2, New(), socket); err == nil {
		t.Fatal("second Serve should refuse while a daemon is already listening")
	}
}

// waitForDaemon polls Version until the listener is up.
func waitForDaemon(t *testing.T, c *Client) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		ver, err := c.Version(ctx)
		cancel()
		if err == nil {
			return ver
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
