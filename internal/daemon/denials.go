package daemon

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
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

// denialScanSink writes the run's log through to its file while scanning each
// line for the egress proxy's markers: "egress denied:" hosts feed a tool
// error, and "egress pending:"/"egress allowed:" drive the approval event feed.
type denialScanSink struct {
	w      io.WriteCloser
	runID  string
	den    *egressDenials
	pend   *pendingEgress
	events *eventBus
	buf    bytes.Buffer
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
		s.den.record(s.runID, host)
		return
	}
	if host, ok := parseEgressHost(line, "egress pending: "); ok {
		if s.pend != nil && s.pend.add(s.runID, host) {
			s.publish(Event{
				Type:   EventEgressPending,
				RunID:  s.runID,
				Target: host,
				Detail: "mcpvessel egress allow " + s.runID + " " + host,
			})
		}
		return
	}
	if host, ok := parseEgressHost(line, "egress allowed: "); ok {
		if s.pend != nil {
			s.pend.remove(s.runID, host)
		}
		s.publish(Event{Type: EventEgressApproved, RunID: s.runID, Target: host})
	}
}

func (s *denialScanSink) publish(e Event) {
	if s.events != nil {
		e.Time = nowFunc()
		s.events.publish(e)
	}
}

func (s *denialScanSink) Close() error { return s.w.Close() }

// parseEgressHost pulls the host that follows marker in an
// "<marker><host> (agent ...)" proxy line.
func parseEgressHost(line, marker string) (string, bool) {
	i := strings.Index(line, marker)
	if i < 0 {
		return "", false
	}
	host, _, _ := strings.Cut(line[i+len(marker):], " ")
	host = strings.TrimSpace(host)
	return host, host != ""
}

// enrichEgressError appends the cage's blocked hosts to a tool error, so the
// caller learns the failure was the cage denying egress and how to allow it.
func enrichEgressError(err error, hosts []string) error {
	if err == nil || len(hosts) == 0 {
		return err
	}
	joined := strings.Join(hosts, ",")
	return fmt.Errorf("%w\nthe cage blocked this server from reaching %s; allow it with 'mcpvessel run/serve --egress %s', or bake it into the Vesselfile with EGRESS allow:%s and a rebuild",
		err, strings.Join(hosts, ", "), joined, joined)
}
