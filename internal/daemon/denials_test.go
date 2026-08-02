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
	_, _ = sink.Write([]byte("m (agent github) - add it...\n"))
	_, _ = sink.Write([]byte("egress denied: objects.githubusercontent.com (agent github) - ...\n"))
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

func TestDenialSink_DeniedClearsPending(t *testing.T) {
	den := newEgressDenials()
	pend := newPendingEgress()
	sink := &denialScanSink{w: nopWriteCloser{}, runID: "run-1", den: den, pend: pend}

	// A held host is pending until it is decided.
	_, _ = sink.Write([]byte("egress pending: api.example (agent x)\n"))
	if got := pend.list()["run-1"]; len(got) != 1 || got[0] != "api.example" {
		t.Fatalf("pending after pending line = %v, want [api.example]", got)
	}

	// A denial (a rejection or a lapsed hold) clears it from pending and records it.
	_, _ = sink.Write([]byte("egress denied: api.example (agent x) - ...\n"))
	if got := pend.list()["run-1"]; len(got) != 0 {
		t.Errorf("pending after denial = %v, want cleared", got)
	}
	if got := den.hosts("run-1"); len(got) != 1 || got[0] != "api.example" {
		t.Errorf("denials = %v, want [api.example]", got)
	}
}

func TestDenialSink_AllowedClearsDenial(t *testing.T) {
	den := newEgressDenials()
	pend := newPendingEgress()
	sink := &denialScanSink{w: nopWriteCloser{}, runID: "run-1", den: den, pend: pend}

	// A served proxy denies a host on first contact (fail fast)...
	_, _ = sink.Write([]byte("egress denied: api.example (agent x) - ...\n"))
	if got := den.hosts("run-1"); len(got) != 1 || got[0] != "api.example" {
		t.Fatalf("denials after denial = %v, want [api.example]", got)
	}
	// ...then the operator approves it, so it must no longer read as blocked.
	_, _ = sink.Write([]byte("egress allowed: api.example (agent x)\n"))
	if got := den.hosts("run-1"); got != nil {
		t.Errorf("denials after approval = %v, want cleared", got)
	}
}

func TestDenialSink_IgnoresMalformedHost(t *testing.T) {
	den := newEgressDenials()
	pend := newPendingEgress()
	sink := &denialScanSink{w: nopWriteCloser{}, runID: "run-1", den: den, pend: pend}

	// A host carrying bytes outside the hostname charset (here a terminal
	// escape sequence) must not surface to the operator or land in a suggested
	// command; the rule is applied where the host is used, not only where the
	// proxy produced the line.
	_, _ = sink.Write([]byte("egress pending: \x1b[2Jevil.com (agent x)\n"))
	if got := pend.list()["run-1"]; len(got) != 0 {
		t.Errorf("pending after malformed host = %v, want none", got)
	}
	_, _ = sink.Write([]byte("egress denied: \x1b[31mevil.com (agent x) - ...\n"))
	if got := den.hosts("run-1"); got != nil {
		t.Errorf("denials after malformed host = %v, want none", got)
	}
}

func TestEnrichEgressError(t *testing.T) {
	base := errors.New("tool returned an error: connection issue")
	got := enrichEgressError(base, "run-1", []string{"api.github.com"})
	// The client-facing message must name the failure and all three grant
	// scopes: this run only (--once), remembered in config, and baked-in.
	for _, want := range []string{
		"connection issue",
		"mcpvessel egress allow run-1 api.github.com --once",
		"mcpvessel egress allow run-1 api.github.com\n",
		"EGRESS allow:api.github.com",
	} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("enriched error missing %q: %v", want, got)
		}
	}
	// No hosts leaves the error untouched; nil stays nil.
	if enrichEgressError(base, "run-1", nil) != base {
		t.Error("no hosts should return the original error")
	}
	if enrichEgressError(nil, "run-1", []string{"x"}) != nil {
		t.Error("nil error should stay nil")
	}
}

// A caged server that catches the proxy's refusal returns its own message on a
// successful call, so nothing in the result says a cage was involved. The
// daemon knows the run is held and says so in-band, without the server's help.
func TestEgressHoldNote_AnnotatesASuccessfulResult(t *testing.T) {
	res := "Could not get a forecast for 'Toronto': https://geocoding-api.open-meteo.com/v1/search returned HTTP 403"
	got := egressHoldNote(res, "me-weather-a8c1b617", []string{"geocoding-api.open-meteo.com"})

	if !strings.HasPrefix(got, res) {
		t.Errorf("the server's own result must survive intact:\n%s", got)
	}
	for _, want := range []string{
		"[mcpvessel]",
		"geocoding-api.open-meteo.com",
		"not from the host itself",
		"mcpvessel egress allow me-weather-a8c1b617 geocoding-api.open-meteo.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("note missing %q:\n%s", want, got)
		}
	}
}

// Nothing held, nothing said: a working call must not grow a security footer.
func TestEgressHoldNote_SilentWhenNothingHeld(t *testing.T) {
	if got := egressHoldNote("7-day forecast for Toronto", "run-1", nil); got != "7-day forecast for Toronto" {
		t.Errorf("got %q, want the result untouched", got)
	}
}

func TestPendingEgress_HostsForRun(t *testing.T) {
	p := newPendingEgress()
	p.add("run-1", "b.example.com")
	p.add("run-1", "a.example.com")
	p.add("run-2", "c.example.com")

	got := p.hosts("run-1")
	if len(got) != 2 || got[0] != "a.example.com" || got[1] != "b.example.com" {
		t.Errorf("hosts = %v, want both hosts of run-1, sorted", got)
	}
	if got := p.hosts("run-3"); got != nil {
		t.Errorf("hosts = %v, want nil for a run holding nothing", got)
	}
	p.remove("run-1", "a.example.com")
	if got := p.hosts("run-1"); len(got) != 1 || got[0] != "b.example.com" {
		t.Errorf("hosts after remove = %v, want just b.example.com", got)
	}
}
