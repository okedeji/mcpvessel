package mcpregistry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/okedeji/mcpvessel/internal/env"
)

// The registry intermittently accepts a connection, completes TLS, then stalls
// for tens of seconds before answering 200. One stall during first-run setup
// left the docs server permanently unconfigured. A read must abandon the stalled
// attempt and try again rather than wait it out.

// fastRetries shrinks the retry timings so a test proves the loop without
// spending real seconds in it.
func fastRetries(t *testing.T) {
	t.Helper()
	oldTimeout, oldBackoff := readTimeout, readBackoff
	readTimeout, readBackoff = 150*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { readTimeout, readBackoff = oldTimeout, oldBackoff })
}

func TestGet_RetriesAStalledAttempt(t *testing.T) {
	fastRetries(t)
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// Hang past readTimeout, the way the real registry does.
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(serverList{Servers: []serverEnvelope{
			{Server: Server{Name: "io.github.okedeji/mcpvessel-docs", Version: "0.1.2"}},
		}})
	}))
	defer ts.Close()
	t.Setenv(env.MCPRegistry, ts.URL)

	got, err := New().Resolve(context.Background(), "io.github.okedeji/mcpvessel-docs")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Version != "0.1.2" {
		t.Errorf("version = %q, want the entry from the second attempt", got.Version)
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want the stall retried once", n)
	}
}

// A registry that answers has made up its mind. Retrying a 404 only delays the
// same answer, so a status code ends the loop.
func TestGet_DoesNotRetryAnAnsweredRequest(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer ts.Close()
	t.Setenv(env.MCPRegistry, ts.URL)

	if _, err := New().Search(context.Background(), "anything", 0); err == nil {
		t.Fatal("want an error from a 404")
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want a status code to end the loop", n)
	}
}

// Publish is not idempotent. Retrying it could double-publish, so a write must
// never ride the read path's retries.
func TestPublish_IsNeverRetried(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			attempts.Add(1)
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	t.Setenv(env.MCPRegistry, ts.URL)

	err := New().Publish(context.Background(), &Server{Name: "io.github.a/fs"}, "tok")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the registry status surfaced", err)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("publish attempted %d times, want exactly one", n)
	}
}

// Giving up eventually still has to give up, and say how many tries it took.
func TestGet_GivesUpAfterEveryAttemptStalls(t *testing.T) {
	fastRetries(t)
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		<-r.Context().Done()
	}))
	defer ts.Close()
	t.Setenv(env.MCPRegistry, ts.URL)

	_, err := New().Search(context.Background(), "x", 0)
	if err == nil || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("err = %v, want it to report giving up after several attempts", err)
	}
	if n := attempts.Load(); int(n) != readAttempts {
		t.Errorf("attempts = %d, want %d", n, readAttempts)
	}
}
