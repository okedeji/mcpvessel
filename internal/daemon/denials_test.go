package daemon

import (
	"errors"
	"strings"
	"testing"
)

func TestDenialSink_RecordsFromLogLines(t *testing.T) {
	den := newEgressDenials()
	sink := &denialScanSink{w: nopWriteCloser{}, runID: "run-1", den: den}

	// Denial lines arrive split across writes; a partial line must still parse
	// once its newline lands.
	_, _ = sink.Write([]byte("agent starting...\negress denied: api.github.co"))
	_, _ = sink.Write([]byte("m (agent github) — add it...\n"))
	_, _ = sink.Write([]byte("egress denied: objects.githubusercontent.com (agent github) — ...\n"))
	// A plain agent log line is not a denial.
	_, _ = sink.Write([]byte("fetched example.com ok\n"))

	got := den.hosts("run-1")
	want := []string{"api.github.com", "objects.githubusercontent.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("hosts = %v, want %v", got, want)
	}

	den.clear("run-1")
	if den.hosts("run-1") != nil {
		t.Error("clear did not drop the run's denials")
	}
}

func TestEnrichEgressError(t *testing.T) {
	base := errors.New("tool returned an error: connection issue")
	got := enrichEgressError(base, []string{"api.github.com"})
	if !strings.Contains(got.Error(), "connection issue") || !strings.Contains(got.Error(), "EGRESS allow:api.github.com") {
		t.Errorf("enriched error missing parts: %v", got)
	}
	// No hosts leaves the error untouched; nil stays nil.
	if enrichEgressError(base, nil) != base {
		t.Error("no hosts should return the original error")
	}
	if enrichEgressError(nil, []string{"x"}) != nil {
		t.Error("nil error should stay nil")
	}
}
