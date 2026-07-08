package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/progress"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/store"
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

func TestRunBuild_HappyPath(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Agentfile"), []byte(
		"FROM python:3.12-slim\nENTRYPOINT python3 agent.py\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile Agentfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "agent.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile agent.py: %v", err)
	}

	out := filepath.Join(t.TempDir(), "researcher.agent")
	var buf, errBuf bytes.Buffer
	// --no-introspect keeps the packaging path runtime-free.
	if err := runBuild(context.Background(), &buf, &errBuf, buildConfig{srcDir: srcDir, outPath: out, mode: progress.ModePlain, noIntrospect: true}); err != nil {
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
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in output:\n%s", want, stdout)
		}
	}
}

func TestBuildToStore_TagIndexed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTCAGE_HOME", home)
	t.Setenv("AGENTCAGE_REGISTRY", "")

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Agentfile"), []byte(
		"FROM python:3.12-slim\nENTRYPOINT python3 agent.py\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile Agentfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "agent.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile agent.py: %v", err)
	}

	var buf, errBuf bytes.Buffer
	if err := buildToStore(context.Background(), &buf, &errBuf, buildConfig{
		srcDir: srcDir, mode: progress.ModePlain, tag: "@okedeji/researcher:0.1", noIntrospect: true,
	}); err != nil {
		t.Fatalf("buildToStore: %v", err)
	}

	if !strings.Contains(buf.String(), "okedeji/researcher:0.1") {
		t.Errorf("result line should name the ref:\n%s", buf.String())
	}

	st, err := store.New()
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ref, err := reference.Parse("@okedeji/researcher:0.1")
	if err != nil {
		t.Fatalf("reference.Parse: %v", err)
	}
	path, ok, err := st.Get(ref)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !ok {
		t.Fatal("built ref does not resolve in the store")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("resolved store path is not a file: %v", err)
	}
}

func TestValidateEvalSuite(t *testing.T) {
	writeAgentfile := func(dir, eval string) {
		body := "FROM x\nMAIN respond\nENTRYPOINT y\n"
		if eval != "" {
			body = "FROM x\nMAIN respond\nEVAL " + eval + "\nENTRYPOINT y\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "Agentfile"), []byte(body), 0o644); err != nil {
			t.Fatalf("write Agentfile: %v", err)
		}
	}

	t.Run("valid suite passes", func(t *testing.T) {
		src := t.TempDir()
		writeAgentfile(src, "tests/eval.yaml")
		if err := os.MkdirAll(filepath.Join(src, "tests"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "tests", "eval.yaml"),
			[]byte("version: 0.1\ncases:\n  - name: c\n    input:\n      tool: respond\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateEvalSuite(buildConfig{srcDir: src}); err != nil {
			t.Errorf("validateEvalSuite: %v", err)
		}
	})

	t.Run("no EVAL is a no-op", func(t *testing.T) {
		src := t.TempDir()
		writeAgentfile(src, "")
		if err := validateEvalSuite(buildConfig{srcDir: src}); err != nil {
			t.Errorf("validateEvalSuite: %v", err)
		}
	})

	t.Run("missing suite file fails", func(t *testing.T) {
		src := t.TempDir()
		writeAgentfile(src, "tests/eval.yaml")
		err := validateEvalSuite(buildConfig{srcDir: src})
		if err == nil || !strings.Contains(err.Error(), "tests/eval.yaml") {
			t.Errorf("err = %v, want a missing-file error naming the path", err)
		}
	})

	t.Run("invalid yaml fails", func(t *testing.T) {
		src := t.TempDir()
		writeAgentfile(src, "eval.yaml")
		if err := os.WriteFile(filepath.Join(src, "eval.yaml"), []byte("version: 0.1\ncases:\n  - name: c\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := validateEvalSuite(buildConfig{srcDir: src})
		if err == nil || !strings.Contains(err.Error(), "no input.tool") {
			t.Errorf("err = %v, want a schema-validation error", err)
		}
	})

	t.Run("escaping path fails", func(t *testing.T) {
		src := t.TempDir()
		writeAgentfile(src, "../outside.yaml")
		err := validateEvalSuite(buildConfig{srcDir: src})
		if err == nil || !strings.Contains(err.Error(), "escapes the source directory") {
			t.Errorf("err = %v, want an escape error", err)
		}
	})
}

func TestRunBuild_PropagatesBundleError(t *testing.T) {
	// Source dir has no Agentfile, so bundle.Build returns an error.
	srcDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "x.agent")

	var buf, errBuf bytes.Buffer
	err := runBuild(context.Background(), &buf, &errBuf, buildConfig{srcDir: srcDir, outPath: out, mode: progress.ModePlain, noIntrospect: true})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Agentfile not found") {
		t.Errorf("error = %q, want 'Agentfile not found'", err.Error())
	}
}
