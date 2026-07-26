package egress

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"
)

// maxCaptureBody caps how many bytes of one request or response body an
// inspected connection buffers. A body past the cap is forwarded in full but
// captured only up to here, marked truncated, so one large upload cannot grow
// the capture artifact without bound.
const maxCaptureBody = 64 * 1024

// caLifetime bounds the per-run CA's validity. A run outlives it only in the
// pathological case; the CA is ephemeral and dies with the run regardless.
const caLifetime = 24 * time.Hour

// CaptureRecord is one inspected request/response pair on an egress connection.
// Bodies are samples capped at maxCaptureBody. Agent is filled by the proxy,
// not the engine. A Note set with no Method means the connection was tunneled
// without inspection (non-TLS, HTTP/2, a cert the cage rejected, or an upstream
// that failed verification), so the operator learns coverage was incomplete
// rather than assuming silence meant no traffic.
type CaptureRecord struct {
	Host       string
	Agent      string
	Method     string
	URL        string
	ReqHeader  http.Header
	ReqBytes   int64
	ReqBody    []byte
	Status     int
	RespHeader http.Header
	RespBytes  int64
	RespBody   []byte
	Truncated  bool
	Note       string
}

// inspector terminates a cage's TLS in inspect mode, reads the plaintext to
// capture it, and re-encrypts to the real upstream, whose certificate it
// verifies against system roots (the cage no longer can, so the proxy assumes
// that duty). It holds the per-run CA private key, which never leaves the
// proxy; only the CA certificate is injected into cages so they trust the
// leaves it mints.
type inspector struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	emit   func(CaptureRecord)

	leafMu sync.Mutex
	leaves map[string]*tls.Certificate

	// captures holds full records (headers and bodies) for the daemon to pull at
	// teardown into the .replay artifact. The live summary rides emit; these full
	// records never touch the proxy's stdout, so no raw body reaches the run log.
	capMu    sync.Mutex
	captures []CaptureRecord

	// upstreamRoots overrides the roots used to verify the real server, a var
	// for tests that dial an httptest TLS server whose cert is not in the
	// system store. Production leaves it nil, meaning system roots.
	upstreamRoots *x509.CertPool
}

// maxCaptures bounds the full records one run's inspector buffers in memory,
// so a long-lived inspected run cannot grow without bound. The live summary of
// every request still streams regardless; only the artifact's full detail is
// capped, keeping the newest records.
const maxCaptures = 512

// GenerateInspectCA mints an ephemeral CA for a run's egress inspection,
// returning the certificate and private key in PEM. The runtime calls it
// host-side so the cert can be injected into cages (which must trust the
// leaves the proxy mints) before they boot, while the key travels only to the
// egress proxy. The pair dies with the run; it is never persisted.
func GenerateInspectCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating inspect CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generating inspect CA serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mcpvessel egress inspect (ephemeral)"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating inspect CA cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding inspect CA key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// newInspector mints its own ephemeral CA, for tests that need both halves in
// one process. Production splits the CA (GenerateInspectCA host-side) from the
// inspector (newInspectorFromPEM in the proxy).
func newInspector(emit func(CaptureRecord)) (*inspector, error) {
	certPEM, keyPEM, err := GenerateInspectCA()
	if err != nil {
		return nil, err
	}
	return newInspectorFromPEM(certPEM, keyPEM, emit)
}

// newInspectorFromPEM builds an inspector holding the CA the runtime minted and
// handed the proxy. The proxy uses the key to sign the leaves it presents to
// cages; the matching cert was injected into those cages' trust stores.
func newInspectorFromPEM(certPEM, keyPEM []byte, emit func(CaptureRecord)) (*inspector, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("inspect CA cert is not valid PEM")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing inspect CA cert: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("inspect CA key is not valid PEM")
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing inspect CA key: %w", err)
	}
	return &inspector{
		caCert: caCert,
		caKey:  caKey,
		caPEM:  certPEM,
		emit:   emit,
		leaves: map[string]*tls.Certificate{},
	}, nil
}

// leafFor mints (and caches) a leaf certificate for host, signed by the per-run
// CA, so the cage's TLS client accepts the proxy as host. The leaf carries host
// in its SAN, matching what the cage's client verifies against.
func (in *inspector) leafFor(host string) (*tls.Certificate, error) {
	in.leafMu.Lock()
	defer in.leafMu.Unlock()
	if leaf, ok := in.leaves[host]; ok {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(caLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, in.caCert, &key.PublicKey, in.caKey)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: nil}
	in.leaves[host] = leaf
	return leaf, nil
}

// intercept MITMs one already-CONNECTed cage connection to host (host:port in
// target). It probes the upstream first, verifying the real server against
// system roots. An upstream that fails verification is refused (the proxy took
// over the cage's verification duty, so a bad cert is never waved through). An
// upstream that will not speak HTTP/1.1 is tunneled uninspected. Otherwise it
// presents a minted leaf to the cage and proxies HTTP/1.1 request/response
// pairs, capturing each. clientConn is the raw hijacked cage connection after
// the proxy wrote its 200.
func (in *inspector) intercept(clientConn net.Conn, target, host, agent string) {
	defer func() { _ = clientConn.Close() }()

	upstreamTCP, err := dialTarget(target)
	if err != nil {
		in.note(host, agent, "not inspected: "+err.Error())
		return
	}
	upstream := tls.Client(upstreamTCP, &tls.Config{
		ServerName: host,
		RootCAs:    in.upstreamRoots,
		NextProtos: []string{"http/1.1"},
	})
	if err := upstream.Handshake(); err != nil {
		// The real server did not verify, or is not TLS. Failing here is correct:
		// the proxy took over the cage's verification duty, so a bad upstream cert
		// must not be waved through, and the cage's call fails with the note.
		_ = upstream.Close()
		in.note(host, agent, "not inspected: upstream TLS failed ("+err.Error()+")")
		return
	}

	if upstream.ConnectionState().NegotiatedProtocol != "http/1.1" {
		// An HTTP/2-only upstream: we cannot parse and capture it as HTTP/1.1.
		// Fall back to a real pass-through. The cage's TLS is not terminated yet,
		// so clientConn is still clean; close the probe, dial fresh, and pipe the
		// cage straight to the real server, which completes its own TLS there
		// (uninspected but working, and cert-pinning-safe). The probe already
		// verified the real cert, and the cage re-verifies over its own handshake.
		_ = upstream.Close()
		fresh, err := dialTarget(target)
		if err != nil {
			in.note(host, agent, "not inspected: "+err.Error())
			return
		}
		defer func() { _ = fresh.Close() }()
		in.note(host, agent, "not inspected: upstream is HTTP/2, tunneled without capture")
		in.bridge(clientConn, fresh)
		return
	}
	defer func() { _ = upstream.Close() }()

	server := tls.Server(clientConn, &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := host
			if hello.ServerName != "" {
				name = hello.ServerName
			}
			return in.leafFor(name)
		},
		NextProtos: []string{"http/1.1"},
	})
	if err := server.Handshake(); err != nil {
		// The cage rejected our leaf. The likeliest cause is certificate
		// pinning, which cannot be detected before presenting a cert, so the
		// call fails here. Named so the operator is not left guessing.
		in.note(host, agent, "not inspected: cage rejected the inspect certificate (certificate pinning?)")
		return
	}

	in.pump(server, upstream, host, agent)
}

// pump proxies HTTP/1.1 requests from the cage to upstream and responses back,
// capturing each pair. It preserves keep-alive by looping until a read ends the
// connection.
func (in *inspector) pump(cage, upstream net.Conn, host, agent string) {
	cageR := bufio.NewReader(cage)
	upR := bufio.NewReader(upstream)
	for {
		req, err := http.ReadRequest(cageR)
		if err != nil {
			return
		}
		rec := CaptureRecord{Host: host, Agent: agent, Method: req.Method, ReqHeader: req.Header.Clone()}
		if req.URL != nil {
			rec.URL = req.URL.RequestURI()
		}
		req.Body = captureBody(req.Body, &rec.ReqBody, &rec.ReqBytes, &rec.Truncated)
		if err := req.Write(upstream); err != nil {
			in.emitRecord(rec)
			return
		}
		resp, err := http.ReadResponse(upR, req)
		if err != nil {
			in.emitRecord(rec)
			return
		}
		rec.Status = resp.StatusCode
		rec.RespHeader = resp.Header.Clone()
		resp.Body = captureBody(resp.Body, &rec.RespBody, &rec.RespBytes, &rec.Truncated)
		writeErr := resp.Write(cage)
		in.emitRecord(rec)
		if writeErr != nil || req.Close || resp.Close {
			return
		}
	}
}

// bridge pipes bytes both ways between two raw connections without parsing, for
// a stream we chose not to inspect but must still carry. It reuses pipe so a
// half-close on one side propagates a CloseWrite to the other rather than
// leaving a copy goroutine blocked forever.
func (in *inspector) bridge(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, a, b)
	go pipe(&wg, b, a)
	wg.Wait()
}

func (in *inspector) emitRecord(rec CaptureRecord) {
	// Buffer the full record (headers and bodies) for the artifact pull, then
	// stream the live summary. A note-only record (not inspected) still buffers
	// so the artifact reflects the gap.
	in.capMu.Lock()
	if len(in.captures) >= maxCaptures {
		in.captures = in.captures[1:]
	}
	in.captures = append(in.captures, rec)
	in.capMu.Unlock()
	if in.emit != nil {
		in.emit(rec)
	}
}

// snapshot returns a copy of the buffered full records, for the daemon to pull
// at teardown.
func (in *inspector) snapshot() []CaptureRecord {
	in.capMu.Lock()
	defer in.capMu.Unlock()
	out := make([]CaptureRecord, len(in.captures))
	copy(out, in.captures)
	return out
}

func (in *inspector) note(host, agent, note string) {
	in.emitRecord(CaptureRecord{Host: host, Agent: agent, Note: note})
}

// captureBody wraps a body so reading it (which forwarding does) mirrors up to
// maxCaptureBody bytes into sample and counts the full length in total, setting
// truncated when the body ran past the cap. Nil bodies pass through untouched.
func captureBody(body io.ReadCloser, sample *[]byte, total *int64, truncated *bool) io.ReadCloser {
	if body == nil {
		return body
	}
	buf := &bytes.Buffer{}
	return &captureReadCloser{
		inner:     body,
		buf:       buf,
		sample:    sample,
		total:     total,
		truncated: truncated,
	}
}

type captureReadCloser struct {
	inner     io.ReadCloser
	buf       *bytes.Buffer
	sample    *[]byte
	total     *int64
	truncated *bool
	done      bool
}

func (c *captureReadCloser) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if n > 0 {
		*c.total += int64(n)
		if room := maxCaptureBody - c.buf.Len(); room > 0 {
			take := n
			if take > room {
				take = room
				*c.truncated = true
			}
			c.buf.Write(p[:take])
		} else if n > 0 {
			*c.truncated = true
		}
	}
	if err == io.EOF && !c.done {
		c.done = true
		*c.sample = c.buf.Bytes()
	}
	return n, err
}

func (c *captureReadCloser) Close() error {
	if !c.done {
		c.done = true
		*c.sample = c.buf.Bytes()
	}
	return c.inner.Close()
}
