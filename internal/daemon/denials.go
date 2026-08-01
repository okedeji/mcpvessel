package daemon

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/okedeji/mcpvessel/internal/egress"
)

// egressDenials tracks, per run, the hosts the egress proxy denied. It is fed
// by scanning the proxy events teed into the run's durable log, so a served
// tool error can explain that the cage blocked a host and the calling client
// (or an LLM) can relay it.
type egressDenials struct {
	mu    sync.Mutex
	byRun map[string]map[string]bool
}

func newEgressDenials() *egressDenials {
	return &egressDenials{byRun: map[string]map[string]bool{}}
}

func (e *egressDenials) record(runID, host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	set := e.byRun[runID]
	if set == nil {
		set = map[string]bool{}
		e.byRun[runID] = set
	}
	set[host] = true
}

// hosts returns the denied hosts for a run, sorted, or nil if none.
func (e *egressDenials) hosts(runID string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	set := e.byRun[runID]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// remove drops one host from a run's denied set, so a host approved after it was
// denied stops showing up in the tool error's blocked list.
func (e *egressDenials) remove(runID, host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if set := e.byRun[runID]; set != nil {
		delete(set, host)
	}
}

func (e *egressDenials) clear(runID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.byRun, runID)
}

// pendingEgress tracks, per run, the hosts the egress proxy is currently
// holding for the operator's approval. A host is recorded when its "egress
// pending:" marker is first seen and cleared when it is approved, so an
// operator can list what a run is waiting on.
type pendingEgress struct {
	mu    sync.Mutex
	byRun map[string]map[string]bool
}

func newPendingEgress() *pendingEgress {
	return &pendingEgress{byRun: map[string]map[string]bool{}}
}

// add records host as pending, reporting whether it was newly held (so the
// daemon publishes one event per hold, not one per log line).
func (e *pendingEgress) add(runID, host string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	set := e.byRun[runID]
	if set == nil {
		set = map[string]bool{}
		e.byRun[runID] = set
	}
	if set[host] {
		return false
	}
	set[host] = true
	return true
}

func (e *pendingEgress) remove(runID, host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if set := e.byRun[runID]; set != nil {
		delete(set, host)
	}
}

func (e *pendingEgress) clear(runID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.byRun, runID)
}

// list returns every run's currently-held hosts, sorted per run.
func (e *pendingEgress) list() map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string][]string{}
	for runID, set := range e.byRun {
		if len(set) == 0 {
			continue
		}
		hosts := make([]string, 0, len(set))
		for h := range set {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		out[runID] = hosts
	}
	return out
}

// egressPreviews tracks, per run, the not-yet-approved hosts that have a request
// captured and waiting for the operator's decision (under --egress-inspect). It
// marks which pending hosts the operator can `egress preview` to read before
// approving; the request bodies live in the proxy, not here.
type egressPreviews struct {
	mu    sync.Mutex
	byRun map[string]map[string]bool
}

func newEgressPreviews() *egressPreviews {
	return &egressPreviews{byRun: map[string]map[string]bool{}}
}

func (e *egressPreviews) add(runID, host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	set := e.byRun[runID]
	if set == nil {
		set = map[string]bool{}
		e.byRun[runID] = set
	}
	set[host] = true
}

func (e *egressPreviews) remove(runID, host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if set := e.byRun[runID]; set != nil {
		delete(set, host)
	}
}

func (e *egressPreviews) clear(runID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.byRun, runID)
}

// has reports whether a run's host has a pending preview.
func (e *egressPreviews) has(runID, host string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.byRun[runID][host]
}

// list returns every run's hosts with a pending preview, sorted per run.
func (e *egressPreviews) list() map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string][]string{}
	for runID, set := range e.byRun {
		if len(set) == 0 {
			continue
		}
		hosts := make([]string, 0, len(set))
		for h := range set {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		out[runID] = hosts
	}
	return out
}

// denialScanSink writes the run's log through to its file while scanning each
// line for the egress proxy's markers: "egress denied:" hosts feed a tool
// error, "egress pending:"/"egress allowed:" drive the approval event feed, and
// "egress preview:" holds a captured request for the operator to read before
// approving.
type denialScanSink struct {
	w     io.WriteCloser
	runID string
	den   *egressDenials
	pend  *pendingEgress
	prev  *egressPreviews
	// live reports whether the run is still held. The proxy's log pump is async,
	// so a pending/preview line can arrive after the run finished and cleared its
	// state; skipping the add for a dead run keeps egress ls from listing a host
	// whose proxy is already gone (and so cannot be approved or previewed anyway).
	live   func() bool
	events *eventBus
	// ledger is the durable per-server feed; ref resolves this run to its server
	// ref (the ledger's key), and sample pulls the redacted request for a held
	// host off the log-pump path. All optional; nil disables the durable tee.
	ledger *egressLedger
	ref    func() string
	sample func(host string)
	buf    bytes.Buffer
}

// tracking reports whether the sink should still record pending/preview hosts.
func (s *denialScanSink) tracking() bool {
	return s.live == nil || s.live()
}

// ledgerRecord folds one egress fact into the durable per-server feed, keyed by
// the run's ref. A no-op when the ledger or ref resolver is unset (tests). It
// uses recordOnce because the same line is also seen by the daemon's prompt
// on-demand read of the proxy log, and one attempt must stay one event.
func (s *denialScanSink) ledgerRecord(kind, host, detail string) {
	if s.ledger == nil || s.ref == nil {
		return
	}
	s.ledger.recordOnce(s.ref(), kind, host, detail)
}

// recordEgressLineToLedger folds one proxy log line into the durable feed,
// keyed by ref. It is the on-demand counterpart to the scan sink above: the
// daemon reads the proxy's current log directly (bypassing the buffered stream
// pump) and runs each new line through here, so an attempt reaches the feed
// promptly. It touches only the ledger (recordOnce, and a sample pull for a
// held request), never the live stores or the event bus the sink also drives.
func recordEgressLineToLedger(led *egressLedger, ref, line string, sample func(host string)) {
	if host, ok := parseEgressHost(line, "egress denied: "); ok {
		led.recordOnce(ref, ledgerBlocked, host, "")
		return
	}
	if host, ok := parseEgressHost(line, "egress preview: "); ok {
		led.recordOnce(ref, ledgerHeld, host, "")
		if sample != nil {
			sample(host)
		}
		return
	}
	if host, ok := parseEgressHost(line, "egress pending: "); ok {
		led.recordOnce(ref, ledgerHeld, host, "")
		return
	}
	if host, ok := parseEgressHost(line, "egress allowed: "); ok {
		led.recordOnce(ref, ledgerApproved, host, "")
		return
	}
	if host, ok := parseEgressHost(line, "egress secret: "); ok {
		led.recordOnce(ref, ledgerSecret, host, markerTailAfterAgent(line))
		return
	}
}

func (s *denialScanSink) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.buf.Write(p)
	for {
		data := s.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(data[:idx])
		s.buf.Next(idx + 1)
		s.scan(line)
	}
	return n, err
}

// scan turns one proxy log line into denial tracking and approval events.
func (s *denialScanSink) scan(line string) {
	if host, ok := parseEgressHost(line, "egress denied: "); ok {
		s.ledgerRecord(ledgerBlocked, host, "")
		s.den.record(s.runID, host)
		// A denial resolves a hold (a rejection or a lapsed deadline), so the host
		// is no longer pending. Clearing it keeps `egress ls` from listing a host
		// that already failed; the event lets a watcher see the outcome.
		if s.pend != nil {
			s.pend.remove(s.runID, host)
		}
		if s.prev != nil {
			s.prev.remove(s.runID, host)
		}
		s.publish(Event{Type: EventEgressDenied, RunID: s.runID, Target: host})
		return
	}
	if host, ok := parseEgressHost(line, "egress preview: "); ok {
		// A not-yet-approved request was captured and held. Surface the host like a
		// pending one (egress ls, and the tool error names it for a served run),
		// mark it as previewable so the operator can read the request before
		// deciding, and publish the secret-safe summary. The full request is pulled
		// on demand, never on this line. Skip the tracking adds once the run is
		// gone: its proxy can no longer serve the preview or the approval.
		if s.tracking() {
			if s.pend != nil {
				s.pend.add(s.runID, host)
			}
			s.den.record(s.runID, host)
			if s.prev != nil {
				s.prev.add(s.runID, host)
			}
		}
		// The durable feed records the held host and pulls its redacted request
		// so the content survives the cage being reaped.
		s.ledgerRecord(ledgerHeld, host, "")
		if s.sample != nil {
			s.sample(host)
		}
		s.publish(Event{Type: EventEgressPreview, RunID: s.runID, Target: host, Detail: egressMarkerDetail(line, "egress preview: ")})
		return
	}
	if host, ok := parseEgressHost(line, "egress pending: "); ok {
		s.ledgerRecord(ledgerHeld, host, "")
		if s.pend != nil && s.tracking() && s.pend.add(s.runID, host) {
			s.publish(Event{
				Type:   EventEgressPending,
				RunID:  s.runID,
				Target: host,
				Detail: "mcpvessel egress allow " + s.runID + " " + host + "  (grants this agent; add --all for every agent in the run)",
			})
		}
		return
	}
	if host, ok := parseEgressHost(line, "egress allowed: "); ok {
		s.ledgerRecord(ledgerApproved, host, "")
		// An approval resolves any prior denial for the host, so it no longer
		// belongs in a later tool error's blocked list, nor as a pending preview.
		s.den.remove(s.runID, host)
		if s.pend != nil {
			s.pend.remove(s.runID, host)
		}
		if s.prev != nil {
			s.prev.remove(s.runID, host)
		}
		s.publish(Event{Type: EventEgressApproved, RunID: s.runID, Target: host})
		return
	}
	if host, ok := parseEgressHost(line, "egress secret: "); ok {
		// The proxy detected a granted secret in a captured request and emitted its
		// name only (never the value). Record it as the exfiltration signal in the
		// durable feed, and surface it live.
		name := markerTailAfterAgent(line)
		s.ledgerRecord(ledgerSecret, host, name)
		s.publish(Event{Type: EventEgressSecret, RunID: s.runID, Target: host, Detail: name})
		return
	}
	if host, ok := parseEgressHost(line, "egress inspect: "); ok {
		// The proxy already reduced this to a secret-safe summary (no body, no
		// query value), so the detail is the line's tail after the host, forwarded
		// verbatim to the feed and the log.
		s.publish(Event{Type: EventEgressInspect, RunID: s.runID, Target: host, Detail: egressMarkerDetail(line, "egress inspect: ")})
	}
}

// markerTailAfterAgent returns the text after the "(agent <name>) " part of an
// egress marker line, used to pull the secret name off an "egress secret:" line.
func markerTailAfterAgent(line string) string {
	if i := strings.Index(line, ") "); i >= 0 {
		return strings.TrimSpace(line[i+2:])
	}
	return ""
}

// egressMarkerDetail returns the summary that follows the host in an
// "<marker><host> (agent <name>) ..." line, for the event feed.
func egressMarkerDetail(line, marker string) string {
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+len(marker):])
}

func (s *denialScanSink) publish(e Event) {
	if s.events != nil {
		e.Time = nowFunc()
		s.events.publish(e)
	}
}

func (s *denialScanSink) Close() error { return s.w.Close() }

// parseEgressHost pulls the host that follows marker in an
// "<marker><host> (agent ...)" proxy line. The proxy refuses a malformed host
// before it can appear in a line, but the same charset rule is re-applied
// here: what this parse extracts is echoed to the operator's terminal and
// embedded in a suggested command, so it is validated where it is used, not
// only where it was produced.
func parseEgressHost(line, marker string) (string, bool) {
	i := strings.Index(line, marker)
	if i < 0 {
		return "", false
	}
	host, _, _ := strings.Cut(line[i+len(marker):], " ")
	host = strings.TrimSpace(host)
	return host, egress.ValidHost(host)
}

// enrichEgressError appends the cage's blocked hosts to a tool error, so the
// caller learns the failure was the cage denying egress and how to allow it.
func enrichEgressError(err error, runID string, hosts []string) error {
	if err == nil || len(hosts) == 0 {
		return err
	}
	host := hosts[0]
	more := ""
	if len(hosts) > 1 {
		more = " (repeat for each blocked host)"
	}
	// Ways out, weakest grant first. Each `allow` grants the host to this agent
	// only; add --all to grant every agent in the run. The caller (an operator or
	// an LLM relaying the tool error) picks the scope it wants.
	return fmt.Errorf("%w\nthe cage was blocked from reaching %s. To allow it, choose one:\n"+
		"  this run only:            mcpvessel egress allow %s %s --once%s\n"+
		"  remember for future runs: mcpvessel egress allow %s %s%s\n"+
		"  bake in (and share):      add 'EGRESS allow:%s' to the Vesselfile, then rebuild\n"+
		"  (each allow grants this agent; add --all to grant every agent in the run)",
		err, strings.Join(hosts, ", "),
		runID, host, more,
		runID, host, more,
		strings.Join(hosts, ","))
}
