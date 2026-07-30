// Package egress is the in-run HTTP CONNECT proxy: a cage reaches only the
// hosts its EGRESS allow: policy names, and the internal run network makes
// this the only way out. By default it filters on the CONNECT host without
// terminating TLS, so it holds no secret and never sees a payload. Under the
// operator's opt-in inspect mode (Config.Inspect) it instead terminates the
// cage's TLS to capture what was sent, re-encrypting to the real host whose
// certificate it verifies; see intercept.go.
package egress

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config maps a source (a cage's run-network address) to the hostnames it may
// reach. Default deny: an unknown source or unlisted host is refused. Names
// maps a source address to a human label (the agent's name) for event lines.
type Config struct {
	Sources map[string][]string `json:"sources"`
	Names   map[string]string   `json:"names,omitempty"`
	// NoHold makes an unapproved host fail fast instead of parking the call for
	// the operator. A served agent is driven by a remote MCP client that cannot
	// answer an inline prompt, so holding only stalls it: the client relays the
	// denial, the operator approves, and the client retries. A run/call has an
	// operator at the terminal, so it holds. See the hold deadline in await.
	NoHold bool `json:"no_hold,omitempty"`
	// Inspect turns on TLS interception: the proxy terminates the cage's TLS to
	// read the plaintext, re-encrypting to the real host whose certificate it
	// verifies. Off by default; the non-inspect path never decrypts. When on,
	// InspectCACertPEM/InspectCAKeyPEM carry the per-run CA the runtime minted,
	// whose cert the cages were booted trusting.
	Inspect          bool   `json:"inspect,omitempty"`
	InspectCACertPEM string `json:"inspect_ca_cert_pem,omitempty"`
	InspectCAKeyPEM  string `json:"inspect_ca_key_pem,omitempty"`
	// HoldSeconds bounds how long a held connection waits for the operator's
	// decision before failing closed. Zero uses the built-in default. Applies to
	// the hold path only (NoHold fails fast and never waits).
	HoldSeconds int `json:"hold_seconds,omitempty"`
	// RedactSecrets are the run's granted secret values. When a not-yet-approved
	// request is previewed under inspection, any occurrence of one is replaced by
	// a «NAME» marker before the preview is surfaced, so a reader (an operator, or
	// an agent driving mcpvessel) sees which secret is present without its value.
	// Sent only under inspection, alongside the CA key the proxy already holds;
	// the proxy sees these in plaintext during interception regardless.
	RedactSecrets []SecretValue `json:"redact_secrets,omitempty"`
}

// SecretValue is one granted secret, name and value, used only to redact the
// value from a previewed request.
type SecretValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// holdDeadline bounds how long a CONNECT to an unapproved host waits for the
// operator's decision before failing closed. A human answers, so minutes, and
// the cage is freed rather than parked forever on an unanswered prompt.
const holdDeadline = 3 * time.Minute

// maxPerSource bounds, per source, the concurrent held CONNECTs and the distinct
// hosts a source may have pending at once. A cage that floods the proxy with
// unapproved hosts is denied fast past the cap instead of parking unbounded
// goroutines and growing the hold/pending maps without limit (a DoS).
const maxPerSource = 64

// maxLogged caps the decision-dedup map so a run that touches an unbounded set
// of hosts cannot grow it forever. Past the cap the map is flushed, at worst
// re-logging a line that was already deduped.
const maxLogged = 8192

// waiter is one parked CONNECT: allowed is set before ch closes so the handler
// reads the decision without a second lock. src is the cage that parked it, so
// a per-source approval releases only that cage's waiters.
type waiter struct {
	ch      chan struct{}
	allowed bool
	src     string
}

// Proxy is the in-run CONNECT proxy. Deny-default: a host not in the allow-set
// is held, not refused outright, and the operator approves or rejects it live
// through the control surface. An approval also joins the allow-set, so the
// same host is not held again this run. The static set is the baked, config,
// and per-run --egress hosts known at boot; the runtime set is what the
// operator approved during the run, kept per source so approving a host for one
// cage never opens it for a sibling cage sharing this proxy.
type Proxy struct {
	mu      sync.Mutex
	static  map[string]map[string]bool // src -> host: allowed at boot
	runtime map[string]map[string]bool // src -> host: approved live for that source
	// runtimeAll is the run-wide live allow-set: hosts the operator explicitly
	// approved for every cage with `egress allow --all`. Written only on that
	// explicit choice, never automatically, so the default stays per-source.
	runtimeAll map[string]bool
	requests   map[string]map[string]bool // src -> host: currently pending a decision
	holds      map[string][]*waiter       // host -> CONNECTs parked on a decision
	held       map[string]int             // src -> concurrent held CONNECTs (cap gate)
	names      map[string]string          // src -> agent label
	logged     map[string]bool            // dedup for allowed/denied lines
	events     io.Writer
	deadline   time.Duration
	maxPer     int  // per-source cap on holds and distinct pending hosts
	noHold     bool // served: fail fast instead of parking the call
	// insp, when set, terminates each approved connection's TLS to capture it,
	// re-encrypting to the verified upstream. Nil leaves the proxy a pure
	// pass-through tunnel, the default that never decrypts.
	insp *inspector
	// previews holds, per host, the not-yet-approved request a cage wants to
	// send it, so the operator can see what is about to leave before deciding.
	// Populated only under inspection; pulled over the loopback control surface,
	// never written to stdout, so a body never reaches the durable log.
	previews map[string]*PreviewRequest
	// secretForms are the granted secret values, and their common encodings,
	// paired with the «NAME» marker they are replaced by when a preview is
	// surfaced. Precomputed once so serving a preview is a plain string sweep.
	secretForms []secretForm
}

// New builds a Proxy from cfg, writing decision lines to events.
func New(cfg Config, events io.Writer) *Proxy {
	static := make(map[string]map[string]bool, len(cfg.Sources))
	for src, hosts := range cfg.Sources {
		set := make(map[string]bool, len(hosts))
		for _, h := range hosts {
			// Normalize the same way the incoming CONNECT host is, so an
			// allow-set entry with any uppercase or a trailing dot still
			// matches (otherwise the entry would silently never match and the
			// host would be held/denied though it was explicitly allowed).
			set[normalizeHost(h)] = true
		}
		static[src] = set
	}
	deadline := holdDeadline
	if cfg.HoldSeconds > 0 {
		deadline = time.Duration(cfg.HoldSeconds) * time.Second
	}
	p := &Proxy{
		static:      static,
		runtime:     map[string]map[string]bool{},
		runtimeAll:  map[string]bool{},
		requests:    map[string]map[string]bool{},
		holds:       map[string][]*waiter{},
		held:        map[string]int{},
		names:       cfg.Names,
		logged:      map[string]bool{},
		events:      events,
		deadline:    deadline,
		maxPer:      maxPerSource,
		noHold:      cfg.NoHold,
		previews:    map[string]*PreviewRequest{},
		secretForms: buildSecretForms(cfg.RedactSecrets),
	}
	if cfg.Inspect && cfg.InspectCACertPEM != "" && cfg.InspectCAKeyPEM != "" {
		// A malformed CA is a boot misconfiguration; run without inspection
		// rather than fail the whole proxy, since the tunnel itself is unaffected.
		if insp, err := newInspectorFromPEM([]byte(cfg.InspectCACertPEM), []byte(cfg.InspectCAKeyPEM), p.writeCapture); err == nil {
			p.insp = insp
		} else if events != nil {
			_, _ = fmt.Fprintf(events, "egress inspect: disabled (bad CA: %v)\n", err)
		}
	}
	return p
}

// Handler is the CONNECT proxy for a boot-time config, the shape tests use.
func Handler(cfg Config, events io.Writer) http.Handler { return New(cfg, events).Handler() }

// Handler returns the CONNECT proxy.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "egress proxy only supports CONNECT", http.StatusMethodNotAllowed)
			return
		}
		// Normalize the CONNECT host before matching: DNS is case-insensitive and
		// a fully-qualified name may carry a trailing dot, so API.GITHUB.COM and
		// github.com. must match a lowercase allow-set entry rather than slip past
		// it. The dial still uses the raw r.Host, so the SSRF guard is untouched.
		host := normalizeHost(hostOnly(r.Host))
		src := hostOnly(r.RemoteAddr)
		if !ValidHost(host) {
			// The host string is the caged server's own bytes. Refusing a
			// malformed one here keeps it out of the allow-set match, the hold
			// table, and the event lines the daemon parses and shows the
			// operator, where a control sequence could otherwise ride along.
			http.Error(w, "malformed egress host", http.StatusBadRequest)
			return
		}
		// Under inspection, a not-yet-approved host is not held at the bare CONNECT.
		// Instead the request is captured and shown to the operator before the
		// decision (previewConn), so what a cage is about to send is what is
		// weighed at approval. Everything else keeps the existing path: await
		// resolves the allowed case immediately and holds/fail-fasts an unapproved
		// host, then intercept (approved) or tunnel.
		if allowed, known := p.isAllowed(src, host); known && !allowed && p.insp != nil {
			p.previewConn(w, r.Host, host, src)
			return
		}
		if !p.await(src, host) {
			http.Error(w, "egress to "+host+" not allowed", http.StatusForbidden)
			return
		}
		if p.insp != nil {
			p.interceptConn(w, r.Host, host, p.label(src))
			return
		}
		tunnel(w, r.Host)
	})
}

// isAllowed reports whether src may already reach host (without holding), and
// whether src is a known cage at all. An unknown source is refused by await, not
// previewed.
func (p *Proxy) isAllowed(src, host string) (allowed, known bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	srcSet, known := p.static[src]
	if !known {
		return false, false
	}
	return srcSet[host] || p.runtime[src][host] || p.runtimeAll[host], true
}

// previewConn terminates a not-yet-approved connection's TLS, captures the
// request the cage wants to send without forwarding it, surfaces it (a
// secret-safe line to stdout, the full request over the loopback control pull),
// and gates forwarding on the operator's decision. A run/call holds inline; a
// served run drops the attempt and relies on the client's retry after approval.
func (p *Proxy) previewConn(w http.ResponseWriter, target, host, src string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "egress proxy needs a hijackable connection", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	cage, cageR, req, prev, err := p.insp.terminateAndRead(client, host)
	if err != nil {
		// Pinning, an oversized body, or a read error: the request cannot be
		// previewed. Surface why and drop; nothing was forwarded.
		p.writePreview(src, host, nil, err.Error())
		_ = client.Close()
		return
	}
	defer func() { _ = cage.Close() }()

	p.storePreview(host, prev)
	p.writePreview(src, host, prev, "")

	if p.noHold {
		// Served: no operator in the request path. Record the host as pending so
		// egress ls and the tool error surface it, drop this attempt, and let the
		// client retry once the operator approves out of band. The "egress
		// preview:" line already recorded the host for the daemon; emitting a
		// "denied" here would only make the daemon clear the pending preview, so it
		// is deliberately not marked denied. The cage still sees a failed request.
		p.mu.Lock()
		p.noteRequestLocked(src, host)
		p.mu.Unlock()
		writeCageError(cage, "egress to "+host+" is pending approval; approve it and retry")
		return
	}

	// Run/call: hold the request on a waiter released by the approval control
	// path (decide), or the deadline. This mirrors await's hold but keeps the
	// captured request buffered so it can be replayed on approval.
	p.mu.Lock()
	if p.held[src] >= p.maxPer || p.overCapLocked(src, host) {
		p.mu.Unlock()
		p.clearPreview(host)
		writeCageError(cage, "too many pending egress hosts for this agent")
		p.mark("denied", src, host)
		return
	}
	wtr := &waiter{ch: make(chan struct{}), src: src}
	p.holds[host] = append(p.holds[host], wtr)
	p.held[src]++
	p.noteRequestLocked(src, host)
	p.mu.Unlock()

	var allowed bool
	select {
	case <-wtr.ch:
		allowed = wtr.allowed
	case <-time.After(p.deadline):
		p.removeWaiter(host, wtr)
	}
	p.clearPreview(host)
	if allowed {
		p.mark("allowed", src, host)
		p.insp.forwardPreviewed(cage, cageR, req, target, host, p.label(src))
		return
	}
	p.mark("denied", src, host)
	writeCageError(cage, "egress to "+host+" denied")
}

// writeCageError sends a minimal HTTP/1.1 error response over a terminated cage
// connection, so the cage's HTTP client sees a failed request rather than a
// dropped socket.
func writeCageError(cage net.Conn, msg string) {
	_, _ = fmt.Fprintf(cage, "HTTP/1.1 403 Forbidden\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
}

func (p *Proxy) storePreview(host string, prev *PreviewRequest) {
	p.mu.Lock()
	p.previews[host] = prev
	p.mu.Unlock()
}

func (p *Proxy) getPreview(host string) *PreviewRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.previews[host]
}

func (p *Proxy) clearPreview(host string) {
	p.mu.Lock()
	delete(p.previews, host)
	p.mu.Unlock()
}

// writePreview emits the secret-safe preview line to stdout: method, path with
// the query stripped, and byte count, never a body or query value, so a pending
// request's contents never reach the durable log. The full request is pulled
// separately over the loopback control surface. A note (empty prev) reports why
// a request could not be previewed.
func (p *Proxy) writePreview(src, host string, prev *PreviewRequest, note string) {
	if p.events == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev == nil {
		_, _ = fmt.Fprintf(p.events, "egress preview: %s (agent %s) [%s]\n", host, p.label(src), sanitizeNote(note))
		return
	}
	q := ""
	if i := strings.IndexByte(prev.URL, '?'); i >= 0 {
		q = " +query"
	}
	_, _ = fmt.Fprintf(p.events, "egress preview: %s (agent %s) %s %s%s  %dB\n",
		host, p.label(src), sanitizeMethod(prev.Method), sanitizePath(prev.URL), q, len(prev.Body))
	// If a granted secret is in the captured request, name it (never the value):
	// this is the exfiltration signal the daemon folds into the durable audit feed.
	for _, name := range p.previewSecretNames(prev) {
		_, _ = fmt.Fprintf(p.events, "egress secret: %s (agent %s) %s\n", host, p.label(src), name)
	}
}

// interceptConn hijacks the approved CONNECT, sends the 200, and hands the raw
// cage connection to the inspector, which MITMs it. Mirrors tunnel's preamble;
// the inspector owns the connection and the upstream dial from here.
func (p *Proxy) interceptConn(w http.ResponseWriter, target, host, agent string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "egress proxy needs a hijackable connection", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	p.insp.intercept(client, target, host, agent)
}

// writeCapture renders one capture as a secret-safe summary line on the event
// stream: method, path with the query stripped, byte counts, and status, never
// a body or a query value, so nothing a request carried can leak into the log
// that containerd persists. The full bodies live only in the pulled artifact.
func (p *Proxy) writeCapture(rec CaptureRecord) {
	if p.events == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec.Note != "" {
		_, _ = fmt.Fprintf(p.events, "egress inspect: %s (agent %s) [%s]\n", rec.Host, rec.Agent, sanitizeNote(rec.Note))
		return
	}
	q := ""
	if i := strings.IndexByte(rec.URL, '?'); i >= 0 {
		q = " +query"
	}
	_, _ = fmt.Fprintf(p.events, "egress inspect: %s (agent %s) %s %s%s  %dB out, %d %dB in\n",
		rec.Host, rec.Agent, sanitizeMethod(rec.Method), sanitizePath(rec.URL), q, rec.ReqBytes, rec.Status, rec.RespBytes)
}

// sanitizeNote reduces a not-inspected note to printable ASCII on one line. A
// note may embed a dial or TLS error string that names the caged server's own
// chosen host, so this keeps an attacker-influenced byte from injecting a
// newline or control sequence into the event line the operator's terminal
// renders and the daemon parses.
func sanitizeNote(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= ' ' && c < 0x7f {
			out = append(out, c)
		} else {
			out = append(out, ' ')
		}
	}
	return string(out)
}

// sanitizeMethod keeps only a short uppercase token, so a caged server's own
// request line cannot smuggle bytes into the log through the method.
func sanitizeMethod(m string) string {
	if len(m) > 16 {
		m = m[:16]
	}
	out := make([]byte, 0, len(m))
	for i := 0; i < len(m); i++ {
		if c := m[i]; c >= 'A' && c <= 'Z' {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// sanitizePath strips the query and reduces the path to printable ASCII without
// spaces, capped, so an attacker-chosen request target cannot inject a newline
// or control sequence into the event line the operator's terminal renders.
func sanitizePath(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if len(raw) > 128 {
		raw = raw[:128]
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c > ' ' && c < 0x7f {
			out = append(out, c)
		} else {
			out = append(out, '?')
		}
	}
	if len(out) == 0 {
		return "/"
	}
	return string(out)
}

// await returns true if src may reach host: immediately when the allow-set
// already names it, or after the operator approves the held connection. A
// rejection or a wait past the deadline returns false.
func (p *Proxy) await(src, host string) bool {
	p.mu.Lock()
	srcSet, known := p.static[src]
	if !known {
		// An unregistered source is a misconfiguration or a spoof, not a cage
		// whose host to approve; refuse it outright rather than hold.
		p.mu.Unlock()
		p.mark("denied", src, host)
		return false
	}
	if srcSet[host] || p.runtime[src][host] || p.runtimeAll[host] {
		p.mu.Unlock()
		p.mark("allowed", src, host)
		return true
	}
	if p.noHold {
		// Served: do not park the call. Still surface the host as pending so the
		// operator can approve it (which joins this source's runtime allow-set),
		// and record the denial so the tool error names the host. The client
		// retries after approval and the host then passes. Past the per-source cap
		// a flood of distinct hosts is refused without recording more pending
		// entries, bounding the pending map.
		if p.overCapLocked(src, host) {
			p.mu.Unlock()
			p.mark("denied", src, host)
			return false
		}
		p.noteRequestLocked(src, host)
		p.mu.Unlock()
		p.pending(src, host)
		p.mark("denied", src, host)
		return false
	}
	if p.held[src] >= p.maxPer || p.overCapLocked(src, host) {
		// Too many concurrent holds or distinct pending hosts for this source:
		// deny fast instead of parking another goroutine, so one cage cannot
		// exhaust the proxy's memory by flooding unapproved CONNECTs.
		p.mu.Unlock()
		p.mark("denied", src, host)
		return false
	}
	wtr := &waiter{ch: make(chan struct{}), src: src}
	first := len(p.holds[host]) == 0
	p.holds[host] = append(p.holds[host], wtr)
	p.held[src]++
	p.noteRequestLocked(src, host)
	p.mu.Unlock()

	// One prompt per host while it is held; a re-attempt after a decision asks
	// again, so the marker is emitted on the first waiter, not deduped forever.
	if first {
		p.pending(src, host)
	}

	select {
	case <-wtr.ch:
		if wtr.allowed {
			p.mark("allowed", src, host)
			return true
		}
		p.mark("denied", src, host)
		return false
	case <-time.After(p.deadline):
		p.removeWaiter(host, wtr)
		p.mark("denied", src, host)
		return false
	}
}

// decide resolves CONNECTs held on host. An allow also joins the runtime
// allow-set so later attempts pass without another prompt. It is scoped to a
// source: an approval for one cage never opens the host for a sibling cage
// sharing this proxy.
//
// src identifies the cage. When it is empty the control path could not carry a
// source (the run-control exec passes only the host), so the decision resolves
// for exactly the sources that requested this host, never for every source.
// When src is set, only that source is approved and only its waiters are
// released; other cages holding the same host keep waiting for their own call.
//
// all overrides the scoping: the operator explicitly chose (with
// `egress allow --all`) to open the host for every cage in the run, now and for
// any that request it later.
func (p *Proxy) decide(src, host string, allow, all bool) {
	p.mu.Lock()
	// A decision resolves the host's pending preview: a held run/call clears its
	// own after the waiter returns, but a served preview has no waiter, so drop
	// it here so egress preview stops offering a stale request.
	delete(p.previews, host)
	var released []*waiter
	if all {
		// Explicit run-wide grant: the operator chose to open this host for every
		// cage, so join the run-wide set and release all waiters regardless of
		// source. Only ever reached via `egress allow --all`.
		if allow {
			p.runtimeAll[host] = true
		}
		released = p.holds[host]
		delete(p.holds, host)
		for s := range p.requests {
			delete(p.requests[s], host)
			if len(p.requests[s]) == 0 {
				delete(p.requests, s)
			}
		}
	} else if src == "" {
		if allow {
			for s := range p.requests {
				if p.requests[s][host] {
					p.approveLocked(s, host)
				}
			}
		}
		released = p.holds[host]
		delete(p.holds, host)
		for s := range p.requests {
			delete(p.requests[s], host)
			if len(p.requests[s]) == 0 {
				delete(p.requests, s)
			}
		}
	} else {
		if allow {
			p.approveLocked(src, host)
		}
		kept := p.holds[host][:0]
		for _, w := range p.holds[host] {
			if w.src == src {
				released = append(released, w)
			} else {
				kept = append(kept, w)
			}
		}
		if len(kept) == 0 {
			delete(p.holds, host)
		} else {
			p.holds[host] = kept
		}
		if p.requests[src] != nil {
			delete(p.requests[src], host)
			if len(p.requests[src]) == 0 {
				delete(p.requests, src)
			}
		}
	}
	for _, w := range released {
		if p.held[w.src] > 0 {
			p.held[w.src]--
		}
	}
	p.mu.Unlock()
	for _, w := range released {
		w.allowed = allow
		close(w.ch)
	}
}

// approveLocked joins host to src's runtime allow-set. Caller holds p.mu.
func (p *Proxy) approveLocked(src, host string) {
	set := p.runtime[src]
	if set == nil {
		set = map[string]bool{}
		p.runtime[src] = set
	}
	set[host] = true
}

// noteRequestLocked records host as pending for src, so a later approval can be
// scoped to the sources that actually asked for it. Caller holds p.mu.
func (p *Proxy) noteRequestLocked(src, host string) {
	set := p.requests[src]
	if set == nil {
		set = map[string]bool{}
		p.requests[src] = set
	}
	set[host] = true
}

// overCapLocked reports whether admitting a new distinct pending host would push
// src past its cap. A host already pending for src is not over cap (a re-attempt
// must still resolve). Caller holds p.mu.
func (p *Proxy) overCapLocked(src, host string) bool {
	return !p.requests[src][host] && len(p.requests[src]) >= p.maxPer
}

// removeWaiter drops a timed-out CONNECT from its host's hold list without
// disturbing others still waiting for the same host.
func (p *Proxy) removeWaiter(host string, wtr *waiter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.holds[host]
	for i, w := range list {
		if w == wtr {
			p.holds[host] = append(list[:i], list[i+1:]...)
			if p.held[wtr.src] > 0 {
				p.held[wtr.src]--
			}
			break
		}
	}
	if len(p.holds[host]) == 0 {
		delete(p.holds, host)
	}
	// A lapsed hold with no sibling still waiting on the same host for this
	// source is no longer pending; drop its request so the source's pending cap
	// frees up and a stale entry cannot linger.
	stillHeld := false
	for _, w := range p.holds[host] {
		if w.src == wtr.src {
			stillHeld = true
			break
		}
	}
	if !stillHeld && p.requests[wtr.src] != nil {
		delete(p.requests[wtr.src], host)
		if len(p.requests[wtr.src]) == 0 {
			delete(p.requests, wtr.src)
		}
	}
}

// Control is the loopback-only surface the daemon drives via nerdctl exec to
// approve or reject a held host. It never listens on a run network.
func (p *Proxy) Control() http.Handler {
	mux := http.NewServeMux()
	decide := func(allow bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			host := normalizeHost(hostOnly(r.URL.Query().Get("host")))
			if host == "" {
				http.Error(w, "host is required", http.StatusBadRequest)
				return
			}
			// Optional src/agent scopes the approval to one cage. src is a raw
			// source key; agent is the operator-facing name (`egress allow --agent
			// NAME`) that the proxy resolves to its source here, since only the
			// proxy holds the name↔source map. With neither supplied the decision
			// resolves for the sources that requested the host (see decide), never
			// for every cage on the proxy. all=true is the explicit run-wide grant.
			src := r.URL.Query().Get("src")
			if agent := r.URL.Query().Get("agent"); agent != "" {
				resolved := p.srcForAgent(agent)
				if resolved == "" {
					http.Error(w, "unknown agent for this run", http.StatusBadRequest)
					return
				}
				src = resolved
			}
			all := r.URL.Query().Get("all") == "true"
			p.decide(src, host, allow, all)
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /allow", decide(true))
	mux.HandleFunc("POST /deny", decide(false))
	// GET /captures returns the full inspected request/response records (headers
	// and bodies) for the daemon to fold into the .replay artifact. Loopback
	// only, like the rest of this surface; a cage has no route to it.
	mux.HandleFunc("GET /captures", func(w http.ResponseWriter, r *http.Request) {
		var recs []CaptureRecord
		if p.insp != nil {
			recs = p.insp.snapshot()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(recs)
	})
	// GET /preview returns the not-yet-approved request (headers and body) a cage
	// wants to send a host, so it can be read before deciding. Loopback only; the
	// body reaches a reader only through this pull, never the stdout event stream
	// a shared log would capture. Granted secret values are redacted to «NAME»
	// here, so a reader (including an agent driving mcpvessel) sees which secret
	// is present without its value; the raw bytes live only in a replay artifact.
	mux.HandleFunc("GET /preview", func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHost(hostOnly(r.URL.Query().Get("host")))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.redactPreview(p.getPreview(host)))
	})
	return mux
}

// pending writes the marker the daemon turns into an approval prompt. Not
// deduped: a fresh attempt after a timeout should prompt again. The write is
// under the lock so concurrent CONNECTs never interleave on the event stream.
func (p *Proxy) pending(src, host string) {
	if p.events == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.events, "egress pending: %s (agent %s)\n", host, p.label(src))
}

// mark writes an allow/deny decision line once per (kind, host), keeping the
// "egress denied: <host> (agent <name>)" prefix the log reader parses.
func (p *Proxy) mark(kind, src, host string) {
	if p.events == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := kind + " " + host
	if p.logged[key] {
		return
	}
	if len(p.logged) >= maxLogged {
		// Flush rather than grow without bound; a re-logged line is harmless.
		p.logged = map[string]bool{}
	}
	p.logged[key] = true
	name := p.label(src)
	switch kind {
	case "allowed":
		_, _ = fmt.Fprintf(p.events, "egress allowed: %s (agent %s)\n", host, name)
	default:
		_, _ = fmt.Fprintf(p.events, "egress denied: %s (agent %s). Approve it for this agent with 'mcpvessel egress allow' (add --all to grant every agent in the run), or bake it into the Vesselfile with EGRESS allow:%s\n", host, name, host)
	}
}

func (p *Proxy) label(src string) string {
	if n := p.names[src]; n != "" {
		return n
	}
	return src
}

// srcForAgent resolves an agent's name (its label, what the operator sees in the
// pending event and `egress ls`) back to its source key. Names are unique per
// run (startEgressProxy refuses two agents on one source), so the match is
// unambiguous. Returns "" when no cage on this proxy carries the name.
func (p *Proxy) srcForAgent(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for src, n := range p.names {
		if n == name {
			return src
		}
	}
	return ""
}

// tunnel dials the target and copies bytes both ways until either side
// closes. It joins its two copy goroutines before returning; none outlives
// the request.
func tunnel(w http.ResponseWriter, target string) {
	upstream, err := dialTarget(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "egress proxy needs a hijackable connection", http.StatusInternalServerError)
		_ = upstream.Close()
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, upstream, client)
	go pipe(&wg, client, upstream)
	wg.Wait()
	_ = upstream.Close()
	_ = client.Close()
}

// dialTarget is a var so tests can tunnel to a loopback backend that
// dialPublic would correctly refuse. Production always uses dialPublic.
var dialTarget = dialPublic

// dialPublic resolves the host and dials only a public address, refusing
// private, loopback, and link-local ones. Without this, an allowed hostname
// resolving to an internal IP (directly or via DNS rebinding) is an SSRF
// pivot into a sibling cage, a gateway, or the host. It dials the address it
// checked, never re-resolving, so a name cannot rebind between check and
// dial.
func dialPublic(target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("malformed egress target %q", target)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolving egress host %s", host)
	}
	for _, ip := range ips {
		if isPublic(ip) {
			return net.Dial("tcp", net.JoinHostPort(ip.String(), port))
		}
	}
	return nil, fmt.Errorf("egress host %s resolves to no public address", host)
}

// reservedCIDRs are non-global ranges net.IP's own predicates miss: they are
// not loopback/private/link-local/multicast, yet must never be dialed. Covers
// CGNAT, IETF protocol assignments, benchmarking, 6to4 relay anycast, reserved
// space, NAT64, and documentation blocks.
var reservedCIDRs = func() []*net.IPNet {
	nets := make([]*net.IPNet, 0, 8)
	for _, c := range []string{
		"0.0.0.0/8",      // RFC1122 "this network" (0.x is a localhost alias on some kernels)
		"100.64.0.0/10",  // RFC6598 CGNAT
		"192.0.0.0/24",   // RFC6890 IETF protocol assignments
		"192.0.2.0/24",   // RFC5737 documentation (TEST-NET-1)
		"198.18.0.0/15",  // RFC2544 benchmarking
		"192.88.99.0/24", // RFC7526 6to4 relay anycast (deprecated)
		"240.0.0.0/4",    // RFC1112 reserved (includes 255.255.255.255 broadcast)
		"64:ff9b::/96",   // RFC6052 NAT64 well-known prefix
		"2001:db8::/32",  // RFC3849 IPv6 documentation
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// isPublic is allowlist-posture: only a global unicast address that is not
// private and not in any reserved range is dialable. Starting from
// IsGlobalUnicast (which already rejects loopback, link-local, multicast, and
// the unspecified address) closes the gap that a denylist of a few predicates
// left open. Without this, an allowed hostname resolving to CGNAT, NAT64, or
// another reserved range is an SSRF pivot.
func isPublic(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, n := range reservedCIDRs {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// normalizeHost lowercases a host and strips a single trailing dot so the
// allow-set and the incoming CONNECT host compare on equal footing (DNS names
// are case-insensitive and "example.com." is the same host as "example.com").
func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// ValidHost bounds a CONNECT host to hostname and IP-literal characters. A
// name is matched against allow-sets, keyed into the hold table, and written
// to the event stream the daemon renders in the operator's terminal, so it
// must never carry spaces, control bytes, or escape sequences. Exported for
// the daemon, which applies the same rule when it parses a host back out of a
// proxy event line.
func ValidHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.' || c == '-' || c == ':' || c == '_':
		default:
			return false
		}
	}
	return true
}
