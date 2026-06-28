package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/history"
)

func TestWriteMetrics(t *testing.T) {
	d := New()
	store, err := history.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d.hist = store

	for _, r := range []history.Record{
		{RunID: "r1", Status: history.StatusSucceeded, CostMicroUSD: 12_000},
		{RunID: "r2", Status: history.StatusSucceeded, CostMicroUSD: 8_000},
		{RunID: "r3", Status: history.StatusFailed},
	} {
		if err := store.Put(r); err != nil {
			t.Fatal(err)
		}
	}

	var b strings.Builder
	d.writeMetrics(&b)
	out := b.String()

	for _, want := range []string{
		`agentcage_runs_total{status="succeeded"} 2`,
		`agentcage_runs_total{status="failed"} 1`,
		"agentcage_runs_live 0",
		"# TYPE agentcage_cages_live gauge",
		"agentcage_cost_usd_total 0.020000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q in:\n%s", want, out)
		}
	}
}
