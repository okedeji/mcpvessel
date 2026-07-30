package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/env"
)

// The egress ledger is the durable, per-server change feed behind `mcpvessel
// audit`. The daemon captures egress facts continuously, even while no agent is
// watching, and folds them here keyed by the server's ref (stable across the
// ephemeral per-client instances). At session start an agent reads the
// unsurfaced tail plus the rolling summary, reports it, then acks: the daemon
// records the agent's new summary and prunes what was acked. This is the WAL
// checkpoint the agent commits after consuming the feed.

// maxLedgerEventsPerRef caps a ref's unpruned events, so a server that floods
// new hosts without ever being acked cannot grow the file without bound. Past
// the cap the oldest events drop; they are the ones most likely already folded
// into an earlier summary anyway.
const maxLedgerEventsPerRef = 500

// ledgerSampleBodyCap bounds the captured request body kept per event, so the
// ledger stays a compact digest, not a full traffic recording (that is what a
// .replay artifact is for).
const ledgerSampleBodyCap = 4 * 1024

// AuditEvent is one recorded egress fact for a server. Repeats of the same
// (kind, host) collapse into one entry with a bumped Count, so the feed reads as
// "blocked exfil.attacker.net x3", not three lines.
type AuditEvent struct {
	Seq       int64     `json:"seq"` // the change-feed offset; the ack cursor is a Seq
	Kind      string    `json:"kind"`
	Host      string    `json:"host"`
	Detail    string    `json:"detail,omitempty"` // secret name for kind=secret; free text otherwise
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Sample is the redacted request the cage wanted to send this host, kept so an
	// agent can judge the content long after the cage was reaped. Granted secrets
	// are already «NAME» here; raw bytes live only in a .replay artifact.
	Sample *egress.PreviewRequest `json:"sample,omitempty"`
}

// Ledger event kinds.
const (
	ledgerBlocked  = "blocked"  // a host the cage was denied
	ledgerHeld     = "held"     // a host held for approval (a new-host decision)
	ledgerApproved = "approved" // a host that was approved, resolving a hold
	ledgerSecret   = "secret"   // a granted secret was detected leaving toward a host
)

// serverLedger is one server's stored feed: the rolling agent-written summary of
// what was already surfaced, plus every event, with those at or below Cursor
// already surfaced (kept until pruned).
type serverLedger struct {
	Ref     string       `json:"ref"`
	Summary string       `json:"summary,omitempty"`
	Cursor  int64        `json:"cursor"`
	Events  []AuditEvent `json:"events,omitempty"`
}

// AuditServer is one server's feed as `mcpvessel audit` reports it: the rolling
// summary of what was already surfaced plus the unsurfaced events since the last
// ack. Cursor is the watermark to pass back to `audit ack`. Serving and Secrets
// are filled in by the CLI/daemon from live state, not the ledger.
type AuditServer struct {
	Ref     string       `json:"ref"`
	Serving bool         `json:"serving,omitempty"`
	Secrets []string     `json:"secrets,omitempty"`
	Summary string       `json:"summary,omitempty"`
	Cursor  int64        `json:"cursor"`
	Events  []AuditEvent `json:"events,omitempty"`
}

// egressLedger is the durable store, one JSON file, keyed by ref.
type egressLedger struct {
	mu      sync.Mutex
	path    string
	seq     int64
	servers map[string]*serverLedger
	now     func() time.Time
}

// ledgerPath returns the ledger file under VESSEL_HOME.
func ledgerPath() (string, error) {
	home, err := env.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "egress-ledger.json"), nil
}

// newEgressLedger loads the ledger at path, or starts empty when it is absent or
// unreadable (a corrupt ledger must never wedge the daemon; the worst case is a
// lost history, which the logs still hold).
func newEgressLedger(path string) *egressLedger {
	l := &egressLedger{path: path, servers: map[string]*serverLedger{}, now: time.Now}
	if buf, err := os.ReadFile(path); err == nil {
		var stored struct {
			Seq     int64                    `json:"seq"`
			Servers map[string]*serverLedger `json:"servers"`
		}
		if json.Unmarshal(buf, &stored) == nil && stored.Servers != nil {
			l.seq = stored.Seq
			l.servers = stored.Servers
		}
	}
	return l
}

// record folds one egress fact into ref's feed. Repeats of the same unsurfaced
// (kind, host) bump the count instead of appending, keeping the feed compact.
func (l *egressLedger) record(ref, kind, host, detail string) {
	if ref == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.servers[ref]
	if s == nil {
		s = &serverLedger{Ref: ref}
		l.servers[ref] = s
	}
	now := l.now()
	for i := range s.Events {
		e := &s.Events[i]
		if e.Seq > s.Cursor && e.Kind == kind && e.Host == host && e.Detail == detail {
			e.Count++
			e.LastSeen = now
			l.save()
			return
		}
	}
	l.seq++
	s.Events = append(s.Events, AuditEvent{
		Seq: l.seq, Kind: kind, Host: host, Detail: detail,
		Count: 1, FirstSeen: now, LastSeen: now,
	})
	l.trimLocked(s)
	l.save()
}

// attachSample stores the redacted captured request on the most recent unsurfaced
// held event for (ref, host), so the agent can review the content. Best-effort:
// the event is created by record on the "egress preview:" marker, and the sample
// is pulled and attached asynchronously.
func (l *egressLedger) attachSample(ref, host string, sample *egress.PreviewRequest) {
	if ref == "" || sample == nil {
		return
	}
	sample = capSample(sample)
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.servers[ref]
	if s == nil {
		return
	}
	for i := len(s.Events) - 1; i >= 0; i-- {
		e := &s.Events[i]
		if e.Seq > s.Cursor && e.Host == host && e.Kind == ledgerHeld {
			e.Sample = sample
			l.save()
			return
		}
	}
}

// feed returns every server's rolling summary and unsurfaced events, keyed by
// ref, for `mcpvessel audit`.
func (l *egressLedger) feed() []AuditServer {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AuditServer, 0, len(l.servers))
	for _, s := range l.servers {
		unsurfaced := make([]AuditEvent, 0, len(s.Events))
		for _, e := range s.Events {
			if e.Seq > s.Cursor {
				unsurfaced = append(unsurfaced, e)
			}
		}
		out = append(out, AuditServer{Ref: s.Ref, Summary: s.Summary, Cursor: l.maxSeqLocked(s), Events: unsurfaced})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// ack commits the agent's consumption of ref's feed: it records the new rolling
// summary and prunes events at or before cursor (the ones the agent saw and
// folded in). Events that arrived after the read keep their place for next time.
func (l *egressLedger) ack(ref string, cursor int64, summary string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.servers[ref]
	if s == nil {
		return fmt.Errorf("no ledger for %q", ref)
	}
	if cursor > s.Cursor {
		s.Cursor = cursor
	}
	s.Summary = summary
	kept := s.Events[:0]
	for _, e := range s.Events {
		if e.Seq > s.Cursor {
			kept = append(kept, e)
		}
	}
	s.Events = kept
	l.save()
	return nil
}

// maxSeqLocked is the highest event seq for a server, the cursor an agent acks
// after reading. Zero when the server has no events.
func (l *egressLedger) maxSeqLocked(s *serverLedger) int64 {
	var max int64
	for _, e := range s.Events {
		if e.Seq > max {
			max = e.Seq
		}
	}
	if max < s.Cursor {
		return s.Cursor
	}
	return max
}

// trimLocked drops the oldest events past the per-ref cap.
func (l *egressLedger) trimLocked(s *serverLedger) {
	if len(s.Events) > maxLedgerEventsPerRef {
		s.Events = s.Events[len(s.Events)-maxLedgerEventsPerRef:]
	}
}

// save writes the ledger atomically. A failure is logged, not fatal: the feed is
// best-effort observability, never on a run's critical path.
func (l *egressLedger) save() {
	if l.path == "" {
		return
	}
	stored := struct {
		Seq     int64                    `json:"seq"`
		Servers map[string]*serverLedger `json:"servers"`
	}{Seq: l.seq, Servers: l.servers}
	buf, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, l.path)
}

// capSample trims a captured request to a compact digest: the body to
// ledgerSampleBodyCap, marking the trim.
func capSample(p *egress.PreviewRequest) *egress.PreviewRequest {
	out := *p
	if len(out.Body) > ledgerSampleBodyCap {
		out.Body = append([]byte(nil), out.Body[:ledgerSampleBodyCap]...)
		out.Truncated = true
	}
	return &out
}
