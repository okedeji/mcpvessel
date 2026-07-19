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
	p.decide(backendHost, true) // operator approves

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
	p.decide("evil.test", false) // operator rejects
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
	p.decide("api.test", true)
	if !p.await(srcIP, "api.test") {
		t.Error("approved host was not admitted on retry")
	}
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
