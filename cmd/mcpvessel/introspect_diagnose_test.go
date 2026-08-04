package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// The real output from importing pypi:mcp-server-time, which is one of the
// servers the packaged skill tells an agent to reach for by coordinate.
const mcpTwoCrash = `[introspect] Traceback (most recent call last):
[introspect]   File "/usr/local/bin/mcp-server-time", line 5, in <module>
[introspect]     from mcp_server_time import main
[introspect]   File "/usr/local/lib/python3.12/site-packages/mcp_server_time/server.py", line 12, in <module>
[introspect]     from mcp.shared.exceptions import McpError
[introspect] ImportError: cannot import name 'McpError' from 'mcp.shared.exceptions'
`

func TestDiagnoseStartup_NamesTheMcpTwoBreakage(t *testing.T) {
	got := diagnoseStartup(mcpTwoCrash)
	if got == "" {
		t.Fatal("diagnoseStartup said nothing about the mcp 2.0 crash")
	}
	// The whole value of the message is the concrete pin, not "check your deps".
	if !strings.Contains(got, "mcp<2") {
		t.Fatalf("diagnoseStartup = %q, want the mcp<2 pin", got)
	}
}

func TestDiagnoseStartup_FallsBackToTheGenericDependencyCase(t *testing.T) {
	got := diagnoseStartup("ModuleNotFoundError: No module named 'httpx'")
	if got == "" || strings.Contains(got, "mcp<2") {
		t.Fatalf("diagnoseStartup = %q, want the generic Python dependency remedy", got)
	}
}

func TestDiagnoseStartup_RecognizesTheNodeCase(t *testing.T) {
	if got := diagnoseStartup("Error: Cannot find module 'zod'"); got == "" {
		t.Fatal("diagnoseStartup said nothing about a missing Node module")
	}
}

// Guessing wrong about why a server died is worse than the raw error, so an
// unrecognized failure must stay silent and leave the existing hint in place.
func TestDiagnoseStartup_SilentOnAnythingElse(t *testing.T) {
	for _, out := range []string{"", "server exited with status 1", "connection refused"} {
		if got := diagnoseStartup(out); got != "" {
			t.Fatalf("diagnoseStartup(%q) = %q, want silence", out, got)
		}
	}
}

func TestTailWriter_ForwardsEverythingAndKeepsTheTail(t *testing.T) {
	var sink bytes.Buffer
	tw := newTailWriter(&sink)

	// More than the cap, so the tail has to drop the front.
	big := strings.Repeat("a", startupTailBytes*3)
	if _, err := tw.Write([]byte(big)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := tw.Write([]byte(mcpTwoCrash)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Forwarding is the writer's first duty: the caller still sees the build.
	if sink.Len() != len(big)+len(mcpTwoCrash) {
		t.Fatalf("forwarded %d bytes, want %d", sink.Len(), len(big)+len(mcpTwoCrash))
	}
	if got := tw.tail(); len(got) > startupTailBytes {
		t.Fatalf("tail kept %d bytes, want at most %d", len(got), startupTailBytes)
	}
	if !strings.Contains(tw.tail(), "McpError") {
		t.Fatal("tail dropped the crash, which is the only part that diagnoses anything")
	}
}

// The runtime pumps stdout and stderr from separate goroutines into this
// writer, so concurrent writes must not race.
func TestTailWriter_ConcurrentWrites(t *testing.T) {
	tw := newTailWriter(&bytes.Buffer{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_, _ = tw.Write([]byte("line of output\n"))
			}
		}()
	}
	wg.Wait()
	if tw.tail() == "" {
		t.Fatal("tail is empty after concurrent writes")
	}
}
