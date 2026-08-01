package egress

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testHandler builds the proxy with no event sink; tests that assert on
// denial lines call Handler directly with a buffer.
func testHandler(cfg Config) http.Handler { return Handler(cfg, nil) }

// shortProxy builds a proxy whose holds time out fast, so a test that expects a
// denial does not wait the production deadline.
func shortProxy(cfg Config, events io.Writer) *Proxy {
	p := New(cfg, events)
	p.deadline = 100 * time.Millisecond
	return p
}

// waitHold blocks until a CONNECT has parked on host, so a test can approve or
// reject it without racing the parking goroutine.
func waitHold(t *testing.T, p *Proxy, host string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		p.mu.Lock()
		n := len(p.holds[host])
		p.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("host %s was never held", host)
}

// connect sends a raw CONNECT for target and returns the status line plus the
// source IP the proxy saw, so a test can allow or deny exactly it.
func connect(t *testing.T, proxyAddr, target string) (status string, srcIP string, conn net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	srcIP = c.LocalAddr().(*net.TCPAddr).IP.String()
	_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	return line, srcIP, c
}

func TestHandler_RefusesUnknownSource(t *testing.T) {
	srv := httptest.NewServer(testHandler(Config{Sources: map[string][]string{}}))
	defer srv.Close()
	status, _, c := connect(t, srv.Listener.Addr().String(), "example.com:443")
	_ = c.Close()
	if !contains(status, "403") {
		t.Errorf("unknown source status = %q, want 403", status)
	}
}

func TestValidHost(t *testing.T) {
	for _, h := range []string{"example.com", "api.example.com", "10.0.0.8", "::1", "my_host", "a-b.c"} {
		if !ValidHost(h) {
			t.Errorf("ValidHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"", "evil com", "evil\x1b[2J.com", "host\nname", "evil.com/path", strings.Repeat("a", 254)} {
		if ValidHost(h) {
			t.Errorf("ValidHost(%q) = true, want false", h)
		}
	}
}

func TestHandler_RefusesMalformedHost(t *testing.T) {
	// A CONNECT host outside the hostname charset is refused before it can be
	// matched, held, or written to the event stream the operator's terminal
	// renders.
	srv := httptest.NewServer(testHandler(Config{Sources: map[string][]string{}}))
	defer srv.Close()
	status, _, c := connect(t, srv.Listener.Addr().String(), "bad,host.com:443")
	_ = c.Close()
	if !contains(status, "400") {
		t.Errorf("malformed host status = %q, want 400", status)
	}
}

func TestHandler_LogsDenialOncePerHost(t *testing.T) {
	// Learn the source IP, then deny it everything and watch the event sink.
	tmp := httptest.NewServer(testHandler(Config{}))
	_, srcIP, c0 := connect(t, tmp.Listener.Addr().String(), "x:1")
	_ = c0.Close()
	tmp.Close()

	var events bytes.Buffer
	cfg := Config{Sources: map[string][]string{srcIP: {}}, Names: map[string]string{srcIP: "github"}}
	p := shortProxy(cfg, &events)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	// Two connects to the same unapproved host: each holds then times out, but
	// the denial line is logged once.
	for i := 0; i < 2; i++ {
		_, _, c := connect(t, srv.Listener.Addr().String(), "objects.githubusercontent.com:443")
		_ = c.Close()
	}
	got := events.String()
	if strings.Count(got, "egress denied:") != 1 {
		t.Errorf("want one deduped denial line, got:\n%s", got)
	}
	if !strings.Contains(got, "objects.githubusercontent.com") || !strings.Contains(got, "agent github") {
		t.Errorf("denial line missing host or agent name:\n%s", got)
	}
	// The denial offers the approval command and the permanent bake-in.
	if !strings.Contains(got, "mcpvessel egress allow") || !strings.Contains(got, "EGRESS allow:") {
		t.Errorf("denial line should offer the approve command and the bake-in:\n%s", got)
	}
	// A held host logs a pending marker before it is denied.
	if !strings.Contains(got, "egress pending: objects.githubusercontent.com") {
		t.Errorf("held host should log a pending line:\n%s", got)
	}
}

func TestHandler_HoldsThenRefusesDisallowedHost(t *testing.T) {
	// Learn the source IP from a throwaway connect, then allow only good.test.
	tmp := httptest.NewServer(testHandler(Config{}))
	_, srcIP, c0 := connect(t, tmp.Listener.Addr().String(), "x:1")
	_ = c0.Close()
	tmp.Close()

	real := httptest.NewServer(shortProxy(Config{Sources: map[string][]string{srcIP: {"good.test"}}}, nil).Handler())
	defer real.Close()
	status, _, c := connect(t, real.Listener.Addr().String(), "bad.test:443")
	_ = c.Close()
	if !contains(status, "403") {
		t.Errorf("disallowed host status = %q, want 403 after the hold times out", status)
	}
}

func TestProxy_HoldThenApproveTunnels(t *testing.T) {
	// A permissive dialer so an approved host actually tunnels to the backend.
	old := dialTarget
	dialTarget = func(target string) (net.Conn, error) { return net.Dial("tcp", target) }
	defer func() { dialTarget = old }()

	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("echo:" + line))
	}()
	backendHost := hostOnly(backend.Addr().String())

	probe := httptest.NewServer(testHandler(Config{}))
	_, srcIP, pc := connect(t, probe.Listener.Addr().String(), "x:1")
	_ = pc.Close()
	probe.Close()

	var events bytes.Buffer
	p := New(Config{Sources: map[string][]string{srcIP: {}}, Names: map[string]string{srcIP: "fetch"}}, &events)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	done := make(chan string, 1)
	go func() {
		status, _, c := connect(t, srv.Listener.Addr().String(), backend.Addr().String())
		defer func() { _ = c.Close() }()
		if contains(status, "200") {
			_, _ = fmt.Fprint(c, "ping\n")
			reply, _ := bufio.NewReader(c).ReadString('\n')
			done <- status + reply
			return
		}
		done <- status
	}()

	waitHold(t, p, backendHost)
	p.decide("", backendHost, true, false) // operator approves

	out := <-done
	if !contains(out, "200") || !strings.Contains(out, "echo:ping") {
		t.Errorf("approved host did not tunnel: %q", out)
	}
}

func TestProxy_HoldThenDeny(t *testing.T) {
	probe := httptest.NewServer(testHandler(Config{}))
	_, srcIP, pc := connect(t, probe.Listener.Addr().String(), "x:1")
	_ = pc.Close()
	probe.Close()

	p := New(Config{Sources: map[string][]string{srcIP: {}}}, nil)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	done := make(chan string, 1)
	go func() {
		status, _, c := connect(t, srv.Listener.Addr().String(), "evil.test:443")
		_ = c.Close()
		done <- status
	}()
	waitHold(t, p, "evil.test")
	p.decide("", "evil.test", false, false) // operator rejects
	if status := <-done; !contains(status, "403") {
		t.Errorf("rejected host status = %q, want 403", status)
	}
}

func TestProxy_NoHoldFailsFastThenApprovalPasses(t *testing.T) {
	// A served proxy must not park an unapproved host: the connect returns 403
	// at once, but the host is still surfaced as pending and recorded as denied
	// so the operator can approve it and the client's retry passes.
	probe := httptest.NewServer(testHandler(Config{}))
	_, srcIP, pc := connect(t, probe.Listener.Addr().String(), "x:1")
	_ = pc.Close()
	probe.Close()

	var events bytes.Buffer
	p := New(Config{Sources: map[string][]string{srcIP: {}}, Names: map[string]string{srcIP: "fetch"}, NoHold: true}, &events)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	// No goroutine and no waitHold: a served proxy fails fast, so the connect
	// returns without anyone approving it.
	start := time.Now()
	status, _, c := connect(t, srv.Listener.Addr().String(), "api.test:443")
	_ = c.Close()
	if !contains(status, "403") {
		t.Fatalf("served host status = %q, want an immediate 403", status)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("served connect took %s, want it to fail fast without holding", elapsed)
	}
	if log := events.String(); !strings.Contains(log, "egress pending: api.test") || !strings.Contains(log, "egress denied: api.test") {
		t.Errorf("fail-fast log = %q, want both a pending and a denied line", log)
	}

	// After the operator approves, the same host passes without holding: the
	// client's retry (modeled here at the decision level) is admitted at once.
	p.decide("", "api.test", true, false)
	if !p.await(srcIP, "api.test") {
		t.Error("approved host was not admitted on retry")
	}
}

func TestProxy_ApprovalIsPerSource(t *testing.T) {
	// Two cages share one proxy. Approving a host for cage A must not open it for
	// cage B: a runtime approval is scoped to the source that requested it.
	const a, b = "10.0.0.2", "10.0.0.3"
	p := New(Config{Sources: map[string][]string{a: {}, b: {}}, NoHold: true}, nil)

	// A hits the host (fail fast records it as pending for A), the operator
	// approves it for A, and A's retry now passes.
	if p.await(a, "api.test") {
		t.Fatal("host should be denied before approval")
	}
	p.decide(a, "api.test", true, false)
	if !p.await(a, "api.test") {
		t.Error("approved host was not admitted for source A")
	}

	// B never requested it and was never approved, so it stays denied: the
	// approval did not leak across cages.
	if p.await(b, "api.test") {
		t.Error("approval for source A leaked to source B")
	}
}

func TestProxy_ApproveAllGrantsEveryCage(t *testing.T) {
	// `egress allow --all` (decide with all=true) opens the host for every cage
	// in the run, including one that never requested it and one that requests it
	// later. This is the explicit opt-in, in contrast to the per-source default.
	const a, b = "10.0.0.2", "10.0.0.3"
	p := New(Config{Sources: map[string][]string{a: {}, b: {}}, NoHold: true}, nil)

	if p.await(a, "api.test") {
		t.Fatal("host should be denied before approval")
	}
	p.decide("", "api.test", true, true) // operator approves for all agents

	if !p.await(a, "api.test") {
		t.Error("run-wide approval did not admit the requesting cage A")
	}
	// B never requested it, yet the explicit --all grant covers it.
	if !p.await(b, "api.test") {
		t.Error("run-wide (--all) approval did not cover cage B")
	}
}

func TestControl_AgentNameScopesToOneSource(t *testing.T) {
	// `egress allow --agent NAME` reaches Control as ?agent=NAME. The proxy
	// resolves the name to that cage's source and scopes the grant to it, never
	// to a sibling cage that shares the proxy.
	const a, b = "10.0.0.2", "10.0.0.3"
	p := New(Config{
		Sources: map[string][]string{a: {}, b: {}},
		Names:   map[string]string{a: "fetcher", b: "sneaky"},
		NoHold:  true,
	}, nil)
	ctrl := httptest.NewServer(p.Control())
	defer ctrl.Close()

	// Both cages request the host so both are pending; only "fetcher" is named.
	if p.await(a, "api.test") || p.await(b, "api.test") {
		t.Fatal("host should be denied before approval")
	}
	resp, err := http.Post(ctrl.URL+"/allow?host=api.test&agent=fetcher", "", nil)
	if err != nil {
		t.Fatalf("control post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("allow status = %d, want 204", resp.StatusCode)
	}

	if !p.await(a, "api.test") {
		t.Error("named agent 'fetcher' (source A) was not granted the host")
	}
	if p.await(b, "api.test") {
		t.Error("grant scoped to 'fetcher' leaked to the unnamed sibling source B")
	}
}

func TestControl_UnknownAgentIsRejected(t *testing.T) {
	// A name no cage carries is a bad request, not a silent no-op that the
	// operator might read as success.
	p := New(Config{
		Sources: map[string][]string{"10.0.0.2": {}},
		Names:   map[string]string{"10.0.0.2": "fetcher"},
		NoHold:  true,
	}, nil)
	ctrl := httptest.NewServer(p.Control())
	defer ctrl.Close()

	resp, err := http.Post(ctrl.URL+"/allow?host=api.test&agent=ghost", "", nil)
	if err != nil {
		t.Fatalf("control post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-agent status = %d, want 400", resp.StatusCode)
	}
}

func TestProxy_HoldCapDeniesInsteadOfParks(t *testing.T) {
	// Past the per-source cap a new unapproved host is denied fast rather than
	// parked, so one cage cannot exhaust the proxy with held CONNECTs.
	const src = "10.0.0.9"
	p := New(Config{Sources: map[string][]string{src: {}}}, nil)
	p.maxPer = 2
	p.deadline = 2 * time.Second

	// Fill the cap with two parked holds on distinct hosts.
	for _, h := range []string{"one.test", "two.test"} {
		go p.await(src, h)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		n := p.held[src]
		p.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("held count reached %d, want 2", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A third distinct host is over cap: it must return false fast, not block for
	// the hold deadline.
	start := time.Now()
	if p.await(src, "three.test") {
		t.Error("over-cap host was allowed, want denied")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("over-cap connect took %s, want a fast deny without parking", elapsed)
	}

	// Release the parked holds so the goroutines exit.
	p.decide("", "one.test", false, false)
	p.decide("", "two.test", false, false)
}

func TestHandler_TunnelsAllowedHost(t *testing.T) {
	// The backend is on loopback, which the SSRF guard rightly refuses;
	// swap in a permissive dialer to exercise the data path.
	old := dialTarget
	dialTarget = func(target string) (net.Conn, error) { return net.Dial("tcp", target) }
	defer func() { dialTarget = old }()

	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("echo:" + line))
	}()

	backendHost := hostOnly(backend.Addr().String())

	probe := httptest.NewServer(testHandler(Config{}))
	_, srcIP, p := connect(t, probe.Listener.Addr().String(), "x:1")
	_ = p.Close()
	probe.Close()

	srv := httptest.NewServer(testHandler(Config{Sources: map[string][]string{srcIP: {backendHost}}}))
	defer srv.Close()

	status, _, c := connect(t, srv.Listener.Addr().String(), backend.Addr().String())
	defer func() { _ = c.Close() }()
	if !contains(status, "200") {
		t.Fatalf("tunnel status = %q, want 200", status)
	}
	_, _ = fmt.Fprint(c, "ping\n")
	reply, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("reading tunneled reply: %v", err)
	}
	if reply != "echo:ping\n" {
		t.Errorf("tunneled reply = %q, want echo:ping", reply)
	}
}

func TestHandler_MatchesHostCaseInsensitively(t *testing.T) {
	// An allow-set entry is lowercase; a CONNECT to the same name uppercased and
	// with a trailing dot must still match, not slip past the filter and hold.
	// Stub the dialer so timing reflects only the match, not a real DNS lookup:
	// a matched host reaches the dial (fast error), an unmatched one holds.
	old := dialTarget
	dialTarget = func(target string) (net.Conn, error) { return nil, fmt.Errorf("stub dial %s", target) }
	defer func() { dialTarget = old }()

	probe := httptest.NewServer(testHandler(Config{}))
	_, srcIP, pc := connect(t, probe.Listener.Addr().String(), "x:1")
	_ = pc.Close()
	probe.Close()

	p := shortProxy(Config{Sources: map[string][]string{srcIP: {"good.test"}}}, nil)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	start := time.Now()
	status, _, c := connect(t, srv.Listener.Addr().String(), "GOOD.TEST.:443")
	_ = c.Close()
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Errorf("connect took %s: host did not match the allow-set and was held", elapsed)
	}
	// A matched-but-undialable host is refused by the dial (403), never held.
	if !contains(status, "403") {
		t.Errorf("status = %q, want 403 from the dial on a matched host", status)
	}
	p.mu.Lock()
	held := len(p.holds["good.test"])
	p.mu.Unlock()
	if held != 0 {
		t.Errorf("matched host was held (%d waiters), want an immediate match", held)
	}
}

func TestHandler_RefusesPrivateTarget(t *testing.T) {
	// The host is explicitly allowed, so a 403 can only come from the dial
	// guard refusing the private address, not from host-deny.
	tmp := httptest.NewServer(testHandler(Config{}))
	_, srcIP, c0 := connect(t, tmp.Listener.Addr().String(), "x:1")
	_ = c0.Close()
	tmp.Close()

	srv := httptest.NewServer(testHandler(Config{Sources: map[string][]string{srcIP: {"10.0.0.1"}}}))
	defer srv.Close()
	status, _, c := connect(t, srv.Listener.Addr().String(), "10.0.0.1:8080")
	_ = c.Close()
	if !contains(status, "403") {
		t.Errorf("private target status = %q, want 403 (SSRF guard)", status)
	}
}

func TestIsPublic(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"10.0.0.1", false},            // RFC1918
		{"192.168.5.2", false},         // RFC1918 (the Lima host)
		{"172.16.0.1", false},          // RFC1918
		{"127.0.0.1", false},           // loopback
		{"169.254.169.254", false},     // link-local (cloud metadata)
		{"0.0.0.0", false},             // unspecified
		{"::1", false},                 // IPv6 loopback
		{"fd00::1", false},             // IPv6 ULA
		{"2606:4700:4700::1111", true}, // public IPv6
		{"100.64.0.1", false},          // RFC6598 CGNAT
		{"198.18.0.1", false},          // RFC2544 benchmarking
		{"192.0.2.1", false},           // TEST-NET-1 documentation
		{"240.0.0.1", false},           // reserved
		{"255.255.255.255", false},     // broadcast
		{"64:ff9b::808:808", false},    // NAT64 well-known prefix
		{"2001:db8::1", false},         // IPv6 documentation
	}
	for _, c := range cases {
		if got := isPublic(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isPublic(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestHandler_RejectsNonConnect(t *testing.T) {
	srv := httptest.NewServer(testHandler(Config{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}

func TestProxy_DenyDropsWithoutReholding(t *testing.T) {
	// After the operator denies a host, a later attempt on it is dropped fast:
	// refused without holding, logged as "dropped" (not held again), and no longer
	// pending. This is what keeps a denied server from re-prompting on every call.
	probe := httptest.NewServer(testHandler(Config{}))
	_, srcIP, pc := connect(t, probe.Listener.Addr().String(), "x:1")
	_ = pc.Close()
	probe.Close()

	var events bytes.Buffer
	p := New(Config{Sources: map[string][]string{srcIP: {}}, Names: map[string]string{srcIP: "fetch"}, NoHold: true}, &events)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	// First attempt: served fail-fast holds it pending; the operator denies it.
	status, _, c := connect(t, srv.Listener.Addr().String(), "evil.test:443")
	_ = c.Close()
	if !contains(status, "403") {
		t.Fatalf("first attempt = %q, want 403", status)
	}
	p.decide("", "evil.test", false, false) // operator denies (source-less, served path)

	// Only assert on what the second attempt logs.
	events.Reset()

	// Second attempt: dropped fast, one "dropped" line, and never held again.
	status2, _, c2 := connect(t, srv.Listener.Addr().String(), "evil.test:443")
	_ = c2.Close()
	if !contains(status2, "403") {
		t.Fatalf("second attempt = %q, want 403", status2)
	}
	log := events.String()
	if !strings.Contains(log, "egress dropped: evil.test") {
		t.Errorf("after deny, log = %q, want an 'egress dropped' line", log)
	}
	if strings.Contains(log, "egress pending: evil.test") {
		t.Errorf("after deny, the host was held again: %q", log)
	}
	p.mu.Lock()
	stillPending := p.requests[srcIP]["evil.test"]
	denied := p.isDeniedLocked(srcIP, "evil.test")
	p.mu.Unlock()
	if stillPending {
		t.Error("a denied host must not stay pending")
	}
	if !denied {
		t.Error("deny was not recorded in the deny-set")
	}
}

func TestProxy_AllowLiftsDeny(t *testing.T) {
	// Deny and allow are mutually exclusive: a later allow overrides an earlier
	// deny, so a host denied by mistake can be reopened.
	const src = "10.0.0.5"
	p := New(Config{Sources: map[string][]string{src: {}}, NoHold: true}, nil)

	p.decide(src, "api.test", false, false) // deny
	p.mu.Lock()
	denied := p.isDeniedLocked(src, "api.test")
	p.mu.Unlock()
	if !denied {
		t.Fatal("host was not denied")
	}

	p.decide(src, "api.test", true, false) // allow lifts the deny
	p.mu.Lock()
	stillDenied := p.isDeniedLocked(src, "api.test")
	allowed := p.runtime[src]["api.test"]
	p.mu.Unlock()
	if stillDenied {
		t.Error("allow did not lift the denial")
	}
	if !allowed {
		t.Error("allow did not open the host")
	}
	if !p.await(src, "api.test") {
		t.Error("host was not admitted after allow")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
