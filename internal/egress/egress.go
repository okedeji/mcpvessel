// Package egress is the in-run HTTP CONNECT proxy: a cage reaches only the
// hosts its EGRESS allow: policy names, and the internal run network makes
// this the only way out. It filters on the CONNECT host without terminating
// TLS, so it holds no secret and never sees a payload.
package egress

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Config maps a source (a cage's run-network address) to the hostnames it may
// reach. Default deny: an unknown source or unlisted host is refused. Names
// maps a source address to a human label (the agent's name) for event lines.
type Config struct {
	Sources map[string][]string `json:"sources"`
	Names   map[string]string   `json:"names,omitempty"`
}

// holdDeadline bounds how long a CONNECT to an unapproved host waits for the
// operator's decision before failing closed. A human answers, so minutes, and
// the cage is freed rather than parked forever on an unanswered prompt.
const holdDeadline = 3 * time.Minute

// waiter is one parked CONNECT: allowed is set before ch closes so the handler
// reads the decision without a second lock.
type waiter struct {
	ch      chan struct{}
	allowed bool
}

// Proxy is the in-run CONNECT proxy. Deny-default: a host not in the allow-set
// is held, not refused outright, and the operator approves or rejects it live
// through the control surface. An approval also joins the allow-set, so the
// same host is not held again this run. The static set is the baked, config,
// and per-run --egress hosts known at boot; the runtime set is what the
// operator approved during the run.
type Proxy struct {
	mu       sync.Mutex
	static   map[string]map[string]bool // src -> host: allowed at boot
	runtime  map[string]bool            // host -> approved live, for any source
	holds    map[string][]*waiter       // host -> CONNECTs parked on a decision
	names    map[string]string          // src -> agent label
	logged   map[string]bool            // dedup for allowed/denied lines
	events   io.Writer
	deadline time.Duration
}

// New builds a Proxy from cfg, writing decision lines to events.
func New(cfg Config, events io.Writer) *Proxy {
	static := make(map[string]map[string]bool, len(cfg.Sources))
	for src, hosts := range cfg.Sources {
		set := make(map[string]bool, len(hosts))
		for _, h := range hosts {
			set[h] = true
		}
		static[src] = set
	}
	return &Proxy{
		static:   static,
		runtime:  map[string]bool{},
		holds:    map[string][]*waiter{},
		names:    cfg.Names,
		logged:   map[string]bool{},
		events:   events,
		deadline: holdDeadline,
	}
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
		host := hostOnly(r.Host)
		src := hostOnly(r.RemoteAddr)
		if !p.await(src, host) {
			http.Error(w, "egress to "+host+" not allowed", http.StatusForbidden)
			return
		}
		tunnel(w, r.Host)
	})
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
	if srcSet[host] || p.runtime[host] {
		p.mu.Unlock()
		p.mark("allowed", src, host)
		return true
	}
	wtr := &waiter{ch: make(chan struct{})}
	first := len(p.holds[host]) == 0
	p.holds[host] = append(p.holds[host], wtr)
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

// decide resolves every CONNECT held on host. An allow also joins the runtime
// allow-set so later attempts pass without another prompt. Driven by the
// control surface.
func (p *Proxy) decide(host string, allow bool) {
	p.mu.Lock()
	if allow {
		p.runtime[host] = true
	}
	list := p.holds[host]
	delete(p.holds, host)
	p.mu.Unlock()
	for _, w := range list {
		w.allowed = allow
		close(w.ch)
	}
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
			break
		}
	}
	if len(p.holds[host]) == 0 {
		delete(p.holds, host)
	}
}

// Control is the loopback-only surface the daemon drives via nerdctl exec to
// approve or reject a held host. It never listens on a run network.
func (p *Proxy) Control() http.Handler {
	mux := http.NewServeMux()
	decide := func(allow bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			host := hostOnly(r.URL.Query().Get("host"))
			if host == "" {
				http.Error(w, "host is required", http.StatusBadRequest)
				return
			}
			p.decide(host, allow)
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /allow", decide(true))
	mux.HandleFunc("POST /deny", decide(false))
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
	p.logged[key] = true
	name := p.label(src)
	switch kind {
	case "allowed":
		_, _ = fmt.Fprintf(p.events, "egress allowed: %s (agent %s)\n", host, name)
	default:
		_, _ = fmt.Fprintf(p.events, "egress denied: %s (agent %s) — approve it with 'mcpvessel egress allow', or bake it into the Vesselfile with EGRESS allow:%s\n", host, name, host)
	}
}

func (p *Proxy) label(src string) string {
	if n := p.names[src]; n != "" {
		return n
	}
	return src
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

func isPublic(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
