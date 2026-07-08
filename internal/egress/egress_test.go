package egress

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	srv := httptest.NewServer(Handler(Config{Sources: map[string][]string{}}))
	defer srv.Close()
	status, _, c := connect(t, srv.Listener.Addr().String(), "example.com:443")
	_ = c.Close()
	if !contains(status, "403") {
		t.Errorf("unknown source status = %q, want 403", status)
	}
}

func TestHandler_RefusesDisallowedHost(t *testing.T) {
	// Learn the source IP from a throwaway connect, then allow only good.test.
	tmp := httptest.NewServer(Handler(Config{}))
	_, srcIP, c0 := connect(t, tmp.Listener.Addr().String(), "x:1")
	_ = c0.Close()
	tmp.Close()

	real := httptest.NewServer(Handler(Config{Sources: map[string][]string{srcIP: {"good.test"}}}))
	defer real.Close()
	status, _, c := connect(t, real.Listener.Addr().String(), "bad.test:443")
	_ = c.Close()
	if !contains(status, "403") {
		t.Errorf("disallowed host status = %q, want 403", status)
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

	probe := httptest.NewServer(Handler(Config{}))
	_, srcIP, p := connect(t, probe.Listener.Addr().String(), "x:1")
	_ = p.Close()
	probe.Close()

	srv := httptest.NewServer(Handler(Config{Sources: map[string][]string{srcIP: {backendHost}}}))
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
	tmp := httptest.NewServer(Handler(Config{}))
	_, srcIP, c0 := connect(t, tmp.Listener.Addr().String(), "x:1")
	_ = c0.Close()
	tmp.Close()

	srv := httptest.NewServer(Handler(Config{Sources: map[string][]string{srcIP: {"10.0.0.1"}}}))
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
	srv := httptest.NewServer(Handler(Config{}))
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
