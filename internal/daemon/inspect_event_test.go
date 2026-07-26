package daemon

import (
	"testing"
	"time"
)

// An "egress inspect:" summary line from the proxy publishes an
// EventEgressInspect carrying the host and the secret-safe detail, without
// touching the denial or pending sets (an inspected request to an approved host
// is neither held nor blocked).
func TestDenialSink_PublishesInspectEvent(t *testing.T) {
	bus := newEventBus()
	ch, unsub := bus.subscribe()
	defer unsub()

	sink := &denialScanSink{w: nopWriteCloser{}, runID: "run-1", den: newEgressDenials(), pend: newPendingEgress(), events: bus}
	_, _ = sink.Write([]byte("egress inspect: gist.github.com (agent notes) POST /gists +query  512B out, 201 128B in\n"))

	select {
	case e := <-ch:
		if e.Type != EventEgressInspect {
			t.Fatalf("event type = %q, want %q", e.Type, EventEgressInspect)
		}
		if e.Target != "gist.github.com" {
			t.Errorf("target = %q, want gist.github.com", e.Target)
		}
		if e.Detail != "gist.github.com (agent notes) POST /gists +query  512B out, 201 128B in" {
			t.Errorf("detail = %q", e.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inspect event published")
	}

	// An inspected request is not a denial and not pending.
	if len(sink.den.hosts("run-1")) != 0 {
		t.Error("inspect line must not record a denial")
	}
	if len(sink.pend.list()["run-1"]) != 0 {
		t.Error("inspect line must not record a pending host")
	}
}
