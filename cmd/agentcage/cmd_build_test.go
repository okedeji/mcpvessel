package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/progress"
)

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1500, "1.5 KB"},
		{1<<20 - 1, "1024.0 KB"},
		{1 << 20, "1.0 MB"},
		{12_400_000, "11.8 MB"},
		{1 << 30, "1.0 GB"},
	}
	for _, tc := range cases {
		got := humanSize(tc.in)
		if got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDefaultOutputPath(t *testing.T) {
	// Compare against the cwd's basename when given ".".
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	wantCWD := filepath.Base(wd) + ".agent"
	if got := defaultOutputPath("."); got != wantCWD {
		t.Errorf("defaultOutputPath(\".\") = %q, want %q", got, wantCWD)
	}

	// Absolute paths take their final segment.
	if got := defaultOutputPath("/Users/foo/researcher"); got != "researcher.agent" {
		t.Errorf("defaultOutputPath(abs) = %q, want researcher.agent", got)
	}

	// Trailing slashes are handled by filepath.Base/Abs.
	if got := defaultOutputPath("./my-agent/"); !strings.HasSuffix(got, "my-agent.agent") {
		t.Errorf("defaultOutputPath trailing-slash = %q, want ...my-agent.agent", got)
	}
}

func TestRunBuild_HappyPath(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Agentfile"), []byte(
		"BASE python:3.12-slim\nENTRYPOINT python3 agent.py\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile Agentfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "agent.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile agent.py: %v", err)
	}

	out := filepath.Join(t.TempDir(), "researcher.agent")
	var buf bytes.Buffer
	if err := runBuild(&buf, srcDir, out, progress.ModePlain); err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	if _, err := os.Stat(out); err != nil {
		t.Errorf("bundle not created at %s: %v", out, err)
	}
	stdout := buf.String()
	for _, want := range []string{
		"Step 1/3 : Parsing Agentfile",
		"Step 2/3 : Hashing source tree",
		"Step 3/3 : Sealing bundle",
		"Successfully built",
		"researcher.agent",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in output:\n%s", want, stdout)
		}
	}
}

func TestRunBuild_PropagatesBundleError(t *testing.T) {
	// Source dir has no Agentfile, so bundle.Build returns an error.
	srcDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "x.agent")

	var buf bytes.Buffer
	err := runBuild(&buf, srcDir, out, progress.ModePlain)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Agentfile not found") {
		t.Errorf("error = %q, want 'Agentfile not found'", err.Error())
	}
}
