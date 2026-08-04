package egress

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// previewProxy builds an inspecting proxy allowing nothing (so every host is
// unapproved and takes the preview path), with a loopback dialer for tests.
func previewProxy(t *testing.T, noHold bool, events io.Writer) *Proxy {
	t.Helper()
	certPEM, keyPEM, err := GenerateInspectCA()
	if err != nil {
		t.Fatalf("GenerateInspectCA: %v", err)
	}
	p := New(Config{
		Sources:          map[string][]string{"127.0.0.1": {}}, // known source, no allowed hosts
		Names:            map[string]string{"127.0.0.1": "notes"},
		NoHold:           noHold,
		Inspect:          true,
		InspectCACertPEM: string(certPEM),
		InspectCAKeyPEM:  string(keyPEM),
	}, events)
	if p.insp == nil {
		t.Fatal("inspector not built")
	}
	p.insp.upstreamRoots = x509.NewCertPool() // no upstream is trusted by default
	return p
}

// cageDial opens a CONNECT to authority through the proxy and returns a TLS
// client over it (ServerName sni) that trusts the proxy's inspect CA, i.e. what
// a caged HTTP client does.
func cageDial(t *testing.T, proxyAddr, authority, sni string, caPEM []byte) *tls.Conn {
	t.Helper()
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_, _ = raw.Write([]byte("CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n\r\n"))
	br := bufio.NewReader(raw)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT not established: %q", status)
	}
	for {
		line, _ := br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return tls.Client(raw, &tls.Config{ServerName: sni, RootCAs: pool, NextProtos: []string{"http/1.1"}})
}

// A served (fail-fast) preview captures the cage's request without forwarding
// it, offers the full body over the loopback /preview pull, keeps it off the
// stdout line, and fails the cage's call.
func TestPreview_ServedCapturesWithoutForwarding(t *testing.T) {
	const host = "gist.github.com"
	const secret = "sk-live-STRIPE-EXFIL-123"

	var forwarded int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	var events strings.Builder
	var emu sync.Mutex
	p := previewProxy(t, true, &lockedWriter{w: &events, mu: &emu})
	// Let the proxy's upstream dial reach the httptest server on loopback and
	// trust its cert, so if a forward ever happened the test would see it.
	restore := loopbackDial()
	defer restore()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	p.insp.upstreamRoots = pool

	proxySrv := &http.Server{Handler: p.Handler()}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = proxySrv.Serve(ln) }()
	defer func() { _ = proxySrv.Close() }()

	control := httptest.NewServer(p.Control())
	defer control.Close()

	cage := cageDial(t, ln.Addr().String(), host+":443", host, p.insp.caPEM)
	body := `{"note":"buy milk","api_key":"` + secret + `"}`
	req, _ := http.NewRequest("POST", "https://"+host+"/gists", strings.NewReader(body))
	if err := req.Write(cage); err != nil {
		t.Fatalf("cage write: %v", err)
	}
	// The served preview fails the call: the cage reads back a 403.
	resp, err := http.ReadResponse(bufio.NewReader(cage), req)
	if err != nil {
		t.Fatalf("cage read: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("served preview status = %d, want 403", resp.StatusCode)
	}
	_ = cage.Close()

	if forwarded != 0 {
		t.Fatalf("previewed request was forwarded upstream (%d times); must be captured-not-forwarded", forwarded)
	}

	// The stdout line is secret-safe.
	emu.Lock()
	line := events.String()
	emu.Unlock()
	if !strings.Contains(line, "egress preview: "+host) {
		t.Errorf("no preview line emitted: %q", line)
	}
	if strings.Contains(line, secret) {
		t.Fatalf("preview line leaked the body: %q", line)
	}

	// The full request, with the secret, is available over the loopback pull.
	prevResp, err := http.Get(control.URL + "/preview?host=" + host)
	if err != nil {
		t.Fatalf("GET /preview: %v", err)
	}
	var prev PreviewRequest
	_ = json.NewDecoder(prevResp.Body).Decode(&prev)
	_ = prevResp.Body.Close()
	if prev.Method != "POST" || prev.URL != "/gists" {
		t.Errorf("preview = %s %s, want POST /gists", prev.Method, prev.URL)
	}
	if !strings.Contains(string(prev.Body), secret) {
		t.Fatalf("preview body did not carry the secret: %.120q", prev.Body)
	}
}

// A held (run/call) preview blocks until the operator decides; on approve the
// buffered request is replayed upstream, on deny nothing is sent.
func TestPreview_HeldForwardsOnApproveDropsOnDeny(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allow      bool
		wantUpHits int32
	}{
		{"approve replays", true, 1},
		{"deny drops", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upHits int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upHits++
				_, _ = w.Write([]byte("pong"))
			}))
			defer upstream.Close()

			p := previewProxy(t, false, io.Discard)
			restore := loopbackDial()
			defer restore()
			pool := x509.NewCertPool()
			pool.AddCert(upstream.Certificate())
			p.insp.upstreamRoots = pool
			p.deadline = 3 * time.Second

			proxySrv := &http.Server{Handler: p.Handler()}
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			go func() { _ = proxySrv.Serve(ln) }()
			defer func() { _ = proxySrv.Close() }()

			// Connect to the upstream's real loopback address so an approved
			// forward actually reaches it; the httptest cert covers 127.0.0.1.
			authority := upstream.Listener.Addr().String()
			host := "127.0.0.1"
			cage := cageDial(t, ln.Addr().String(), authority, host, p.insp.caPEM)
			req, _ := http.NewRequest("POST", "https://"+authority+"/x", strings.NewReader("ping"))
			done := make(chan *http.Response, 1)
			go func() {
				_ = req.Write(cage)
				resp, _ := http.ReadResponse(bufio.NewReader(cage), req)
				done <- resp
			}()

			// Wait for the request to be captured and held, then decide.
			deadline := time.Now().Add(3 * time.Second)
			for p.getPreview("", host) == nil && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if p.getPreview("", host) == nil {
				t.Fatal("request was not captured and held")
			}
			p.decide("127.0.0.1", host, tc.allow, false)

			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("cage call did not return after the decision")
			}
			_ = cage.Close()
			if upHits != tc.wantUpHits {
				t.Errorf("upstream hits = %d, want %d", upHits, tc.wantUpHits)
			}
		})
	}
}
