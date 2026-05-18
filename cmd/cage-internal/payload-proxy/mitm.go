package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// loadCA reads a PEM-encoded CA certificate and private key from disk.
func loadCA(certPath, keyPath string) (*x509.Certificate, *rsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading CA cert: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("no PEM block in CA cert")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA cert: %w", err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading CA key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("no PEM block in CA key")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA key: %w", err)
	}

	return cert, key, nil
}

var (
	leafCache    sync.Map // hostname → *tls.Certificate
	leafKey      *rsa.PrivateKey
	leafKeyOnce  sync.Once
	leafKeyErr   error
	leafFlightMu sync.Mutex
	leafInFlight = make(map[string]chan struct{})
)

// sharedLeafKey lazily generates a single 2048-bit RSA key shared by
// every leaf cert this process issues. Per-hostname cert generation
// then only signs (microseconds) instead of also generating a key
// (hundreds of ms). Without this, parallel cold-start TLS interception
// of the first few requests easily exceeds an agent's HTTP timeout.
func sharedLeafKey() (*rsa.PrivateKey, error) {
	leafKeyOnce.Do(func() {
		leafKey, leafKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	return leafKey, leafKeyErr
}

// leafCert returns a TLS certificate for hostname, signed by the CA.
// Certificates are cached for the lifetime of the process. Concurrent
// cache misses for the same hostname are deduplicated so only one
// goroutine signs while the others wait on the result.
func leafCert(hostname string, ca *x509.Certificate, caKey *rsa.PrivateKey) (*tls.Certificate, error) {
	if cached, ok := leafCache.Load(hostname); ok {
		return cached.(*tls.Certificate), nil
	}

	// Singleflight by hostname: first goroutine generates, others wait.
	leafFlightMu.Lock()
	if wait, ok := leafInFlight[hostname]; ok {
		leafFlightMu.Unlock()
		<-wait
		if cached, ok := leafCache.Load(hostname); ok {
			return cached.(*tls.Certificate), nil
		}
		// In-flight finished without populating the cache (error path).
		// Fall through and retry; rare.
	} else {
		wait := make(chan struct{})
		leafInFlight[hostname] = wait
		leafFlightMu.Unlock()
		defer func() {
			leafFlightMu.Lock()
			delete(leafInFlight, hostname)
			leafFlightMu.Unlock()
			close(wait)
		}()
	}

	// Double-check after acquiring the slot in case another goroutine
	// populated the cache between our miss and our claim.
	if cached, ok := leafCache.Load(hostname); ok {
		return cached.(*tls.Certificate), nil
	}

	priv, err := sharedLeafKey()
	if err != nil {
		return nil, fmt.Errorf("generating leaf key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, ca, &priv.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("creating leaf cert: %w", err)
	}

	leaf := &tls.Certificate{
		Certificate: [][]byte{certDER, ca.Raw},
		PrivateKey:  priv,
	}
	leafCache.Store(hostname, leaf)
	return leaf, nil
}

// extractSNI reads the TLS ClientHello from br without consuming
// bytes (uses Peek). Returns the SNI hostname or "" if not found.
func extractSNI(br *bufio.Reader) string {
	// TLS record: type(1) + version(2) + length(2) + payload
	header, err := br.Peek(5)
	if err != nil || header[0] != 0x16 {
		return ""
	}
	recordLen := int(binary.BigEndian.Uint16(header[3:5]))
	if recordLen > 16384 || recordLen < 42 {
		return ""
	}

	// Peek the full record (header + payload).
	data, err := br.Peek(5 + recordLen)
	if err != nil {
		return ""
	}
	payload := data[5:]

	// ClientHello: type(1) + length(3) + version(2) + random(32)
	if payload[0] != 0x01 {
		return ""
	}
	helloLen := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if helloLen > len(payload)-4 {
		return ""
	}
	hello := payload[4 : 4+helloLen]

	pos := 2 + 32 // skip version + random

	// Session ID
	if pos >= len(hello) {
		return ""
	}
	sidLen := int(hello[pos])
	pos += 1 + sidLen

	// Cipher suites
	if pos+2 > len(hello) {
		return ""
	}
	csLen := int(binary.BigEndian.Uint16(hello[pos:]))
	pos += 2 + csLen

	// Compression methods
	if pos >= len(hello) {
		return ""
	}
	compLen := int(hello[pos])
	pos += 1 + compLen

	// Extensions
	if pos+2 > len(hello) {
		return ""
	}
	extLen := int(binary.BigEndian.Uint16(hello[pos:]))
	pos += 2
	extEnd := pos + extLen
	if extEnd > len(hello) {
		extEnd = len(hello)
	}

	for pos+4 <= extEnd {
		extType := binary.BigEndian.Uint16(hello[pos:])
		extDataLen := int(binary.BigEndian.Uint16(hello[pos+2:]))
		pos += 4
		if int(extType) == 0 && pos+extDataLen <= extEnd {
			// SNI extension: list length(2) + type(1) + name length(2) + name
			sniData := hello[pos : pos+extDataLen]
			if len(sniData) < 5 {
				return ""
			}
			nameLen := int(binary.BigEndian.Uint16(sniData[3:5]))
			if 5+nameLen > len(sniData) {
				return ""
			}
			return string(sniData[5 : 5+nameLen])
		}
		pos += extDataLen
	}
	return ""
}

// handleTLSConn terminates TLS using a dynamically generated cert,
// then feeds the decrypted HTTP to the handler. The SNI hostname is
// injected into request URLs so the handler can route correctly.
func handleTLSConn(rawConn net.Conn, br *bufio.Reader, ca *x509.Certificate, caKey *rsa.PrivateKey, handler http.Handler, logger logr.Logger) {
	defer func() { _ = rawConn.Close() }()

	sni := extractSNI(br)
	if sni == "" {
		logger.Info("TLS connection without SNI, closing")
		return
	}

	cert, err := leafCert(sni, ca, caKey)
	if err != nil {
		logger.Error(err, "generating leaf cert", "sni", sni)
		return
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
	}

	// Wrap the raw connection (with buffered bytes) in TLS.
	bc := &bufferedConn{Conn: rawConn, reader: br}
	tlsConn := tls.Server(bc, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		logger.Info("TLS handshake failed", "sni", sni, "error", err.Error())
		return
	}
	defer func() { _ = tlsConn.Close() }()

	// Inject the SNI hostname into requests so the handler can
	// match LLM host and the Director can rewrite correctly.
	wrappedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "" {
			r.URL.Host = sni
		}
		if r.URL.Scheme == "" {
			r.URL.Scheme = "https"
		}
		handler.ServeHTTP(w, r)
	})

	srv := &http.Server{Handler: wrappedHandler}
	_ = srv.Serve(&singleListener{conn: tlsConn})
}

// bufferedConn wraps a net.Conn with a bufio.Reader so peeked bytes
// from SNI extraction are replayed into the TLS handshake.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

// singleListener yields one connection then blocks forever.
type singleListener struct {
	conn net.Conn
	once sync.Once
}

func (l *singleListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn != nil {
		return conn, nil
	}
	select {} // block forever after first Accept
}

func (l *singleListener) Close() error   { return nil }
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }
