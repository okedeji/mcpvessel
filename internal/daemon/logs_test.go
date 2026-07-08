package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestOpenRunLogSink(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	sink := openRunLogSink("echo-1")
	_, _ = sink.Write([]byte("agent line\n"))
	_ = sink.Close()

	path, _ := runLogPath("echo-1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	if string(data) != "agent line\n" {
		t.Errorf("log file got %q, want the written line", string(data))
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

	// A written log reads back verbatim.
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
