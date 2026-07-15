package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/okedeji/mcpvessel/internal/identity"
)

func TestStampIdentity_HeadersOnEveryResponse(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	stampIdentity(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))

	h := rec.Header()
	if got := h.Get(headerVersion); got != identity.Version {
		t.Errorf("%s = %q, want %q", headerVersion, got, identity.Version)
	}
	// The test binary exists on disk, so the binary stamp must be present
	// and its mtime parseable.
	if h.Get(headerBinary) == "" {
		t.Fatalf("%s header missing", headerBinary)
	}
	if _, err := strconv.ParseInt(h.Get(headerBinaryMtime), 10, 64); err != nil {
		t.Errorf("%s not an integer: %v", headerBinaryMtime, err)
	}
}

// warnFrom runs checkStale on a fresh client against the given headers and
// returns what was written to the warning writer.
func warnFrom(t *testing.T, h http.Header) string {
	t.Helper()
	var buf bytes.Buffer
	old := StaleWarnWriter
	StaleWarnWriter = &buf
	defer func() { StaleWarnWriter = old }()
	c := &Client{}
	c.checkStale(h)
	return buf.String()
}

func TestCheckStale(t *testing.T) {
	// A stand-in daemon binary whose mtime the headers can agree or disagree
	// with.
	exe := filepath.Join(t.TempDir(), "mcpvessel")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime().Unix()

	headers := func(version, binary string, binaryMtime int64) http.Header {
		h := http.Header{}
		if version != "" {
			h.Set(headerVersion, version)
		}
		if binary != "" {
			h.Set(headerBinary, binary)
			h.Set(headerBinaryMtime, strconv.FormatInt(binaryMtime, 10))
		}
		return h
	}

	cases := []struct {
		name     string
		h        http.Header
		wantWarn bool
	}{
		{"no headers, older daemon", http.Header{}, false},
		{"matching version and mtime", headers(identity.Version, exe, mtime), false},
		{"version mismatch", headers(identity.Version+"-other", exe, mtime), true},
		{"binary rebuilt since daemon start", headers(identity.Version, exe, mtime-60), true},
		{"binary gone from disk", headers(identity.Version, exe+"-missing", mtime), true},
		{"version only, no binary stamp", headers(identity.Version, "", 0), false},
	}
	for _, tc := range cases {
		warned := warnFrom(t, tc.h) != ""
		if warned != tc.wantWarn {
			t.Errorf("%s: warned = %v, want %v", tc.name, warned, tc.wantWarn)
		}
	}
}

func TestCheckStale_WarnsOncePerClient(t *testing.T) {
	var buf bytes.Buffer
	old := StaleWarnWriter
	StaleWarnWriter = &buf
	defer func() { StaleWarnWriter = old }()

	h := http.Header{}
	h.Set(headerVersion, identity.Version+"-other")
	c := &Client{}
	c.checkStale(h)
	c.checkStale(h)
	if got := bytes.Count(buf.Bytes(), []byte("warning")); got != 1 {
		t.Errorf("warning written %d times, want once", got)
	}
}
