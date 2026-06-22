package daemon

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/identity"
)

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

// TestServe_SocketRoundTrip starts the daemon on a real Unix socket, dials it
// with the client, and checks version + ps over the wire, then a clean shutdown
// when the context is cancelled.
func TestServe_SocketRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "agentcage.sock")
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

// TestShutdown_StopsServe locks that the control-plane /shutdown route brings
// the daemon down on its own, with no operator signal: Serve runs against a
// background context, so only the shutdown request can stop it.
func TestShutdown_StopsServe(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "agentcage.sock")
	d := New()
	errc := make(chan error, 1)
	go func() { errc <- Serve(context.Background(), d, socket) }()

	c := Dial(socket)
	waitForDaemon(t, c)

	// The ack can race the socket closing, so the request result is not asserted;
	// Serve returning is the proof it worked.
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

// TestServe_RefusesSecondListener locks the fail-fast behavior: a second daemon
// against a socket a live one already owns errors rather than stomping it.
func TestServe_RefusesSecondListener(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "agentcage.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, New(), socket) }()
	waitForDaemon(t, Dial(socket))

	// A bound deadline so a regression (binding anyway) fails the test instead
	// of blocking on a healthy Serve.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := Serve(ctx2, New(), socket); err == nil {
		t.Fatal("second Serve should refuse while a daemon is already listening")
	}
}

// waitForDaemon polls Version until the listener is up, so the round-trip tests
// do not race the goroutine that binds the socket.
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
