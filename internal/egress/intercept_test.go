package egress

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamPool returns a root pool trusting the httptest TLS server's own cert,
// so the interceptor's upstream verification (system roots in production) has a
// root to validate against in the test.
func upstreamPool(t *testing.T, srv *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// A cage that trusts the per-run CA reaches the real server through the
// interceptor, and the request line plus the response status and bodies are
// captured.
func TestInspector_CapturesRequestAndResponse(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "ping" {
			t.Errorf("upstream got body %q, want ping", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("pong"))
	}))
	defer upstream.Close()
	upstreamHost := upstream.Listener.Addr().String()

	var mu sync.Mutex
	var records []CaptureRecord
	in, err := newInspector(func(rec CaptureRecord) {
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("newInspector: %v", err)
	}
	in.upstreamRoots = upstreamPool(t, upstream)
	restore := loopbackDial()
	defer restore()

	// The cage's SNI is the upstream's ServerName; httptest certs are issued for
	// "example.com" and 127.0.0.1, so dial by the host the cert names.
	serverName := "example.com"
	cageSide, proxySide := net.Pipe()
	go in.intercept(proxySide, upstreamHost, serverName, "tester")

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(in.caPEM) {
		t.Fatal("could not load inspect CA PEM")
	}
	cageTLS := tls.Client(cageSide, &tls.Config{ServerName: serverName, RootCAs: caPool, NextProtos: []string{"http/1.1"}})

	req, _ := http.NewRequest(http.MethodPost, "https://"+serverName+"/notes", strings.NewReader("ping"))
	if err := req.Write(cageTLS); err != nil {
		t.Fatalf("cage write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(cageTLS), req)
	if err != nil {
		t.Fatalf("cage read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || string(body) != "pong" {
		t.Fatalf("cage got %d %q, want 201 pong", resp.StatusCode, body)
	}
	_ = cageTLS.Close()

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(records) > 0 })
	mu.Lock()
	defer mu.Unlock()
	rec := records[0]
	if rec.Method != "POST" || rec.URL != "/notes" {
		t.Errorf("captured %s %s, want POST /notes", rec.Method, rec.URL)
	}
	if string(rec.ReqBody) != "ping" {
		t.Errorf("captured req body %q, want ping", rec.ReqBody)
	}
	if rec.Status != http.StatusCreated || string(rec.RespBody) != "pong" {
		t.Errorf("captured resp %d %q, want 201 pong", rec.Status, rec.RespBody)
	}
	if rec.Agent != "tester" {
		t.Errorf("captured agent %q, want tester", rec.Agent)
	}
	// Headers both ways are captured for the artifact (the request's Host header
	// and the response's Content-Length are always present).
	if rec.ReqHeader.Get("Host") == "" && rec.ReqHeader.Get("User-Agent") == "" {
		t.Errorf("request headers not captured: %v", rec.ReqHeader)
	}
	if rec.RespHeader == nil {
		t.Error("response headers not captured")
	}
	// The full record is buffered for the teardown pull, not just streamed.
	if snap := in.snapshot(); len(snap) != 1 || snap[0].URL != "/notes" {
		t.Errorf("snapshot = %+v, want one record for /notes", snap)
	}
}

// When the real server does not verify against the roots the proxy trusts, the
// connection is not waved through: it is noted as an upstream TLS failure and no
// request is proxied. This is the property that keeps inspect mode from
// becoming a downgrade: the proxy still checks the real server's identity.
func TestInspector_RejectsUnverifiedUpstream(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer upstream.Close()

	var mu sync.Mutex
	var records []CaptureRecord
	in, err := newInspector(func(rec CaptureRecord) {
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("newInspector: %v", err)
	}
	// upstreamRoots left as an empty pool: the real server's cert chains to
	// nothing trusted, so verification must fail.
	in.upstreamRoots = x509.NewCertPool()
	restore := loopbackDial()
	defer restore()

	cageSide, proxySide := net.Pipe()
	go in.intercept(proxySide, upstream.Listener.Addr().String(), "example.com", "tester")

	// The cage handshake never completes because the proxy aborts upstream; the
	// pipe closes. We only assert the recorded note.
	go func() { _, _ = io.Copy(io.Discard, cageSide); _ = cageSide.Close() }()

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(records) > 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 1 || !strings.Contains(records[0].Note, "upstream TLS failed") {
		t.Fatalf("records = %+v, want one upstream-TLS-failed note", records)
	}
	if records[0].Method != "" {
		t.Errorf("a rejected upstream must proxy no request, got method %q", records[0].Method)
	}
}

// loopbackDial points dialTarget at a plain dialer so a test can reach the
// httptest server on 127.0.0.1, which dialPublic refuses by design. It returns
// a restore func. Production always uses dialPublic (the SSRF guard).
func loopbackDial() func() {
	old := dialTarget
	dialTarget = func(target string) (net.Conn, error) { return net.Dial("tcp", target) }
	return func() { dialTarget = old }
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
