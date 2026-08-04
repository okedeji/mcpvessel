package main

import (
	"io"
	"strings"
	"sync"
)

// A server that will not start is the most common way an import fails, and the
// error mcpvessel sees for it is uninformative: the bridge reports that the MCP
// handshake got EOF, because the process died before answering. The reason is
// always in what the process printed on its way down, which streams past the
// caller and is gone by the time the error is rendered. This file keeps the
// tail of that output and turns the recognizable crashes into a remedy.

// startupTailBytes bounds what is kept from an introspection's output. Only the
// crash at the end diagnoses anything, and an image build ahead of it can emit
// megabytes.
const startupTailBytes = 8 * 1024

// tailWriter forwards everything to the underlying writer and keeps the last
// startupTailBytes for diagnosis. Writes come from the runtime's output pumps,
// so it is used from more than one goroutine.
type tailWriter struct {
	w  io.Writer
	mu sync.Mutex
	// buf holds at most 2*startupTailBytes and is halved when it exceeds that,
	// so the tail is kept without copying on every write.
	buf []byte
}

func newTailWriter(w io.Writer) *tailWriter { return &tailWriter{w: w} }

// Write holds the lock across the forward as well as the buffer append. The
// runtime pumps the boot's stdout and stderr from separate goroutines, so an
// unsynchronized forward both races the underlying writer and interleaves two
// streams mid-line in the output the caller reads.
func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*startupTailBytes {
		t.buf = t.buf[len(t.buf)-startupTailBytes:]
	}
	return t.w.Write(p)
}

func (t *tailWriter) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) > startupTailBytes {
		return string(t.buf[len(t.buf)-startupTailBytes:])
	}
	return string(t.buf)
}

// startupFailure is one recognizable way a wrapped server dies before it can
// speak MCP, paired with what the caller should do about it.
type startupFailure struct {
	// markers are substrings of the crash output. Any one of them identifies it.
	markers []string
	// explain is the line added to the error: what broke, whose fault it is,
	// and the shape of the fix.
	explain string
}

// startupFailures are read in order, so a more specific pattern comes first.
var startupFailures = []startupFailure{
	{
		// The dominant case as of the mcp 2.0 release: servers that depended on
		// "mcp" without an upper bound, against a 2.0 that moved or renamed what
		// they import. It hits several of the official reference servers.
		markers: []string{
			"from mcp.",
			"import mcp",
			"No module named 'mcp",
		},
		explain: "the server failed to import the Python 'mcp' package. That package released a 2.0 that moved and renamed parts of its API, and a server that never capped its dependency picks it up and breaks. Pin it in the generated Vesselfile's RUN line (\"mcp<2\") and build again.",
	},
	{
		markers: []string{"ModuleNotFoundError", "ImportError"},
		explain: "the server crashed importing one of its own Python dependencies, so it never started. Add or pin the missing package in the generated Vesselfile's RUN line and build again.",
	},
	{
		markers: []string{"ERR_MODULE_NOT_FOUND", "Cannot find module"},
		explain: "the server crashed resolving one of its own Node dependencies, so it never started. Pin or add the package in the generated Vesselfile's RUN line and build again.",
	},
}

// diagnoseStartup reads a failed introspection's output and returns the remedy
// for the failure it recognizes, or "" when it recognizes none. Recognizing
// nothing is the common case and must stay silent: a wrong guess about why a
// server died is worse than the raw error.
func diagnoseStartup(output string) string {
	for _, f := range startupFailures {
		for _, m := range f.markers {
			if strings.Contains(output, m) {
				return f.explain
			}
		}
	}
	return ""
}
