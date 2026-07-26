package main

import (
	"fmt"
	"io"
	"os"

	"github.com/okedeji/mcpvessel/internal/env"
)

// inspectCAPath is where the bridge materializes the combined trust bundle
// inside the cage. A world-readable path under /tmp so any language runtime the
// server uses can read it.
const inspectCAPath = "/tmp/mcpvessel-inspect-ca.pem"

// systemCABundles are the usual on-disk CA bundle locations across base images
// (Debian/Ubuntu, RHEL/Fedora, Alpine). The bridge prepends the first that
// exists so the cage still trusts real roots for anything not proxied, with the
// inspect CA appended.
var systemCABundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/cert.pem",
}

// applyInspectTrust wires the cage's trust store to the per-run inspection CA
// when VESSEL_INSPECT_CA_PEM is set (it is set only under --egress-inspect).
// It writes a bundle of the detected system roots plus the inspect CA, then
// points the common per-runtime trust env vars at it, so the server accepts the
// leaf the egress proxy presents. A failure is non-fatal and noted: the server
// still runs, its TLS to the proxy just fails, which the proxy reports as a
// not-inspected connection. Env is set before the inner server is spawned, so
// it inherits all of it.
func applyInspectTrust(stderr io.Writer) {
	caPEM := os.Getenv(env.InspectCAPEM)
	if caPEM == "" {
		return
	}
	bundle := buildTrustBundle(readFirstFile(systemCABundles), caPEM)
	if err := os.WriteFile(inspectCAPath, []byte(bundle), 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "[inspect] could not write CA bundle, egress inspection may not decrypt: %v\n", err)
		return
	}
	// SSL_CERT_FILE, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE, GIT_SSL_CAINFO all
	// replace the trust root, so they get the combined bundle. NODE_EXTRA_CA_CERTS
	// appends to Node's built-ins, so it gets the inspect CA alone.
	for _, key := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "GIT_SSL_CAINFO"} {
		_ = os.Setenv(key, inspectCAPath)
	}
	if err := os.WriteFile(inspectCAPath+".node", []byte(caPEM), 0o644); err == nil {
		_ = os.Setenv("NODE_EXTRA_CA_CERTS", inspectCAPath+".node")
	}
	// The reserved-prefix var is not passed to the inner server; unset it so it
	// cannot leak the CA onward through the child's environment.
	_ = os.Unsetenv(env.InspectCAPEM)
}

// buildTrustBundle concatenates the detected system bundle (may be empty) with
// the inspect CA, each newline-terminated so the PEM blocks stay separable.
func buildTrustBundle(systemPEM, caPEM string) string {
	out := ""
	if systemPEM != "" {
		out = ensureTrailingNewline(systemPEM)
	}
	return out + ensureTrailingNewline(caPEM)
}

func ensureTrailingNewline(s string) string {
	if s == "" || s[len(s)-1] == '\n' {
		return s
	}
	return s + "\n"
}

// readFirstFile returns the contents of the first path that reads without
// error, or "" if none do (a scratch image may carry no CA bundle at all).
func readFirstFile(paths []string) string {
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	return ""
}
