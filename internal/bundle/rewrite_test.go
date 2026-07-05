package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRewriteManifest_StampsAndKeepsFilesIntact(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), `FROM python:3.12-slim
MAIN respond
EVAL tests/eval.yaml
ENTRYPOINT python3 agent.py
`)
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")
	writeFile(t, filepath.Join(src, "tests", "eval.yaml"), "version: 0.1\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	at := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	passed, failed := 4, 1
	score := 0.83
	err := RewriteManifest(out, func(m *Manifest) error {
		if m.Evals == nil {
			m.Evals = &Evals{Declared: true}
		}
		m.Evals.LastRunAt = &at
		m.Evals.Passed = &passed
		m.Evals.Failed = &failed
		m.Evals.JudgeScore = &score
		return nil
	})
	if err != nil {
		t.Fatalf("RewriteManifest: %v", err)
	}

	manifest, err := ReadManifest(out)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if manifest.Evals == nil || manifest.Evals.Passed == nil || *manifest.Evals.Passed != 4 {
		t.Fatalf("stamp not applied: %+v", manifest.Evals)
	}
	if manifest.Evals.JudgeScore == nil || *manifest.Evals.JudgeScore != 0.83 {
		t.Errorf("judge score not applied: %+v", manifest.Evals)
	}

	// The files/ tree is untouched, so the files_hash integrity check that
	// Extract enforces still passes: the rewrite only ever touched manifest.json.
	if _, err := Extract(out, t.TempDir()); err != nil {
		t.Errorf("Extract after rewrite failed integrity: %v", err)
	}
}

func TestRewriteManifest_MutateErrorLeavesBundleUntouched(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Agentfile"), "FROM x\nENTRYPOINT y\n")
	writeFile(t, filepath.Join(src, "agent.py"), "print('x')\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	sentinel := errors.New("boom")
	if err := RewriteManifest(out, func(m *Manifest) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the mutate error wrapped", err)
	}

	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read bundle after: %v", err)
	}
	if string(before) != string(after) {
		t.Error("bundle changed despite a mutate error")
	}
	if _, statErr := os.Stat(out + ".tmp"); statErr == nil {
		t.Error("rewrite temp file left behind after a failed mutate")
	}
}
