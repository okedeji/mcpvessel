package progress

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlain_StepsAndDone(t *testing.T) {
	var buf bytes.Buffer
	p := NewPlain(&buf)
	p.Step(1, 3, "Parsing Agentfile")
	p.Step(2, 3, "Hashing source tree")
	p.Step(3, 3, "Sealing bundle")
	p.Done()

	want := "Step 1/3 : Parsing Agentfile\nStep 2/3 : Hashing source tree\nStep 3/3 : Sealing bundle\n"
	if got := buf.String(); got != want {
		t.Errorf("output mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestTTY_RendersHeaderAndSteps(t *testing.T) {
	// NewTTY does not require a real terminal; only width detection falls
	// back, so a bytes.Buffer still captures the output.
	var buf bytes.Buffer
	r := NewTTY(&buf)
	r.Step(1, 3, "Parsing Agentfile")
	r.Step(2, 3, "Hashing source tree")
	r.Step(3, 3, "Sealing bundle")
	r.Done()

	out := buf.String()
	for _, want := range []string{
		"[+] Building",
		"FINISHED",
		"=> [1/3] Parsing Agentfile",
		"=> [2/3] Hashing source tree",
		"=> [3/3] Sealing bundle",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in TTY output:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\033[K") {
		t.Errorf("missing ANSI clear-to-EOL in TTY output (escape codes stripped?)")
	}
}

func TestTTY_DoneIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	r := NewTTY(&buf)
	r.Step(1, 1, "Only step")
	r.Done()
	r.Done() // no panic, no double-close
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{"plain", ModePlain},
		{"tty", ModeTTY},
		{"auto", ModeAuto},
		{"", ModeAuto},
		{"garbage", ModeAuto},
	}
	for _, tc := range cases {
		if got := ParseMode(tc.in); got != tc.want {
			t.Errorf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNew_RespectsExplicitMode(t *testing.T) {
	var buf bytes.Buffer
	if _, ok := New(&buf, ModePlain).(*Plain); !ok {
		t.Errorf("New(_, plain) did not return *Plain")
	}
	if _, ok := New(&buf, ModeTTY).(*TTY); !ok {
		t.Errorf("New(_, tty) did not return *TTY")
	}
}

func TestNew_AutoFallsBackToPlainOnNonTTY(t *testing.T) {
	var buf bytes.Buffer
	if _, ok := New(&buf, ModeAuto).(*Plain); !ok {
		t.Errorf("New(buf, auto) did not return *Plain for non-TTY writer")
	}
}

func TestIsTerminal_FalseForNonFile(t *testing.T) {
	var buf bytes.Buffer
	if IsTerminal(&buf) {
		t.Errorf("IsTerminal(&bytes.Buffer{}) = true, want false")
	}
}
