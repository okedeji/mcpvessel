package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestRunLog_TeesAfterAttach locks the capture boundary: output before the file
// attaches reaches the stream only, output after reaches both, so the durable
// log holds the run's own output without the pre-run-id build noise.
func TestRunLog_TeesAfterAttach(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	var inner bytes.Buffer
	rl := &runLog{inner: &inner}

	_, _ = rl.Write([]byte("before-attach\n"))
	f := attachRunLog(rl, "echo-1")
	if f == nil {
		t.Fatal("attachRunLog returned nil")
	}
	_, _ = rl.Write([]byte("after-attach\n"))
	_ = f.Close()

	if got := inner.String(); got != "before-attach\nafter-attach\n" {
		t.Errorf("stream got %q, want both lines", got)
	}
	path, _ := runLogPath("echo-1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	if string(data) != "after-attach\n" {
		t.Errorf("log file got %q, want only the post-attach line", string(data))
	}
}

func TestHandleRunLogs(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	d := New()

	// A run with no log file is a 404.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runs/nope/logs", nil)
	req.SetPathValue("id", "nope")
	d.handleRunLogs(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing log: status %d, want 404", rec.Code)
	}

	// A written log reads back verbatim, no daemon round-trip needed for the run
	// to be over: the file is the source of truth.
	f, err := openRunLog("echo-1")
	if err != nil {
		t.Fatalf("openRunLog: %v", err)
	}
	_, _ = f.WriteString("hello logs\n")
	_ = f.Close()

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/runs/echo-1/logs", nil)
	req.SetPathValue("id", "echo-1")
	d.handleRunLogs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello logs\n" {
		t.Errorf("body %q, want the log contents", rec.Body.String())
	}
}
