package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/bundle"
)

func buildBundleWithEval(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("Agentfile", "FROM x\nMAIN respond\nEVAL tests/eval.yaml\nENTRYPOINT y\n")
	write("tests/eval.yaml", "version: 0.1\n")

	out := filepath.Join(t.TempDir(), "a.agent")
	if err := bundle.Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return out
}

func TestStamp_WritesResults(t *testing.T) {
	out := buildBundleWithEval(t)
	score := 0.83
	report := &Report{Passed: 4, Failed: 1, JudgeScore: &score}
	at := time.Date(2026, 7, 5, 10, 12, 0, 0, time.UTC)

	if err := Stamp(out, report, at); err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	m, err := bundle.ReadManifest(out)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Evals == nil || m.Evals.Passed == nil || *m.Evals.Passed != 4 || *m.Evals.Failed != 1 {
		t.Fatalf("stamp missing: %+v", m.Evals)
	}
	if m.Evals.JudgeScore == nil || *m.Evals.JudgeScore != 0.83 {
		t.Errorf("judge score = %v, want 0.83", m.Evals.JudgeScore)
	}
	if m.Evals.LastRunAt == nil || !m.Evals.LastRunAt.Equal(at) {
		t.Errorf("last run = %v, want %v", m.Evals.LastRunAt, at)
	}

	// A second stamp overwrites cleanly and the source tree still verifies.
	report2 := &Report{Passed: 5, Failed: 0}
	if err := Stamp(out, report2, at); err != nil {
		t.Fatalf("second Stamp: %v", err)
	}
	m2, _ := bundle.ReadManifest(out)
	if *m2.Evals.Passed != 5 || m2.Evals.JudgeScore != nil {
		t.Errorf("second stamp did not overwrite: %+v", m2.Evals)
	}
	if _, err := bundle.Extract(out, t.TempDir()); err != nil {
		t.Errorf("Extract after stamping failed integrity: %v", err)
	}
}
