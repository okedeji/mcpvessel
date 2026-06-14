package egress

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// connect opens a raw connection to the proxy and sends a CONNECT for target,
// returning the status line. It also reports the source IP the proxy will see
// for this connection, so the test can allow or deny exactly it.
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
	// Learn the source IP from a throwaway connect, then allow only good.test
	// for it and ask for a different host.
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
	// A TCP backend that echoes one line, standing in for an allowed host.
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

	// Learn the source IP, then allow it to reach the backend host.
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
