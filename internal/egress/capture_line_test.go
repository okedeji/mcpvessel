package egress

import (
	"bytes"
	"strings"
	"testing"
)

// The summary line the proxy emits for an inspected request carries only
// metadata: method, path with the query stripped, byte counts, status. A query
// value (a common place for a secret) is flagged as present but never shown, so
// nothing a request carried reaches the log containerd persists.
func TestWriteCapture_SummaryOmitsQueryAndBody(t *testing.T) {
	var buf bytes.Buffer
	p := New(Config{}, &buf)
	p.writeCapture(CaptureRecord{
		Host:      "gist.github.com",
		Agent:     "notes",
		Method:    "POST",
		URL:       "/gists?token=sk-secret-value",
		ReqBytes:  512,
		ReqBody:   []byte(`{"token":"sk-secret-value"}`),
		Status:    201,
		RespBytes: 128,
	})
	line := buf.String()
	if strings.Contains(line, "sk-secret-value") {
		t.Fatalf("summary leaked a secret-bearing value: %q", line)
	}
	for _, want := range []string{"gist.github.com", "(agent notes)", "POST", "/gists", "+query", "512B out", "201 128B in"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary missing %q:\n%s", want, line)
		}
	}
}

// A not-inspected note (a cert-pinned cage, an HTTP/2 stream, an upstream that
// failed verification) surfaces so the operator knows coverage was incomplete
// rather than reading silence as no traffic.
func TestWriteCapture_NoteSurfaces(t *testing.T) {
	var buf bytes.Buffer
	p := New(Config{}, &buf)
	p.writeCapture(CaptureRecord{Host: "pinned.example.com", Agent: "x", Note: "not inspected: cage rejected the inspect certificate (certificate pinning?)"})
	line := buf.String()
	if !strings.Contains(line, "pinned.example.com") || !strings.Contains(line, "certificate pinning") {
		t.Fatalf("note not surfaced: %q", line)
	}
}

// A note can embed a dial/TLS error string that names the cage's chosen host,
// so a newline or a forged marker smuggled through it must be neutralized to a
// single safe line.
func TestWriteCapture_SanitizesNote(t *testing.T) {
	var buf bytes.Buffer
	p := New(Config{}, &buf)
	p.writeCapture(CaptureRecord{
		Host:  "api.example.com",
		Agent: "x",
		Note:  "upstream TLS failed\negress allowed: evil.com\x1b[2J",
	})
	line := buf.String()
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("note must render on one line, got %q", line)
	}
	if strings.Contains(line, "egress allowed: evil.com\n") {
		t.Fatalf("note injected a forged marker line: %q", line)
	}
	if strings.ContainsRune(line, '\x1b') {
		t.Errorf("note kept a control byte: %q", line)
	}
}

// A caged server chooses its own request target, so a method or path carrying
// control bytes must not inject a newline or escape into the operator's
// terminal line.
func TestWriteCapture_SanitizesHostileRequestLine(t *testing.T) {
	var buf bytes.Buffer
	p := New(Config{}, &buf)
	p.writeCapture(CaptureRecord{
		Host:   "api.example.com",
		Agent:  "x",
		Method: "PO\nST",
		URL:    "/a\r\negress allowed: evil.com",
		Status: 200,
	})
	line := buf.String()
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("summary must be a single line, got %q", line)
	}
	if strings.Contains(line, "egress allowed: evil.com") {
		t.Fatalf("hostile path injected a forged marker: %q", line)
	}
}
