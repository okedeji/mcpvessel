package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/history"
	"github.com/okedeji/agentcage/internal/llmgateway"
	"github.com/okedeji/agentcage/internal/telemetry"
)

func TestBuildTrace_GroupsCallsByAgentAndWidensSpans(t *testing.T) {
	at := func(sec int64) int64 { return time.Unix(sec, 0).UnixNano() }
	calls := []llmgateway.CallEvent{
		{Agent: "root", Model: "m", PromptTokens: 10, CompletionTokens: 5, CostMicroUSD: 100, StartUnixNano: at(101), EndUnixNano: at(102)},
		{Agent: "root", Model: "m", CostMicroUSD: 50, StartUnixNano: at(103), EndUnixNano: at(104)},
		{Agent: "web", Model: "m", CostMicroUSD: 25, StartUnixNano: at(105), EndUnixNano: at(106)},
	}
	tr := buildTrace("run-1", time.Unix(100, 0), time.Unix(110, 0), calls)

	if tr.Root.Name != "agentcage.run" || tr.Root.Attributes["run_id"] != "run-1" {
		t.Fatalf("root span wrong: %+v", tr.Root)
	}
	if len(tr.Root.Children) != 2 {
		t.Fatalf("agent spans = %d, want 2", len(tr.Root.Children))
	}

	var rootAgent *telemetry.Span
	for _, a := range tr.Root.Children {
		if a.Attributes["agent"] == "root" {
			rootAgent = a
		}
	}
	if rootAgent == nil {
		t.Fatal("no span for agent root")
	}
	if len(rootAgent.Children) != 2 {
		t.Fatalf("root agent calls = %d, want 2", len(rootAgent.Children))
	}
	// The agent span is widened to cover its first call start and last call end.
	if !rootAgent.Start.Equal(time.Unix(101, 0)) || !rootAgent.End.Equal(time.Unix(104, 0)) {
		t.Errorf("agent window = %v..%v, want 101..104", rootAgent.Start.Unix(), rootAgent.End.Unix())
	}
}

func TestHandleRunTrace(t *testing.T) {
	d := New()
	store, err := history.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d.hist = store

	// A run with no trace is a 404.
	if err := store.Put(history.Record{RunID: "r1", Status: history.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runs/r1/trace", nil)
	req.SetPathValue("id", "r1")
	d.handleRunTrace(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no trace: status %d, want 404", rec.Code)
	}

	// A run with a stored trace returns it verbatim.
	if err := store.Put(history.Record{RunID: "r2", TraceJSON: `{"run_id":"r2"}`}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/runs/r2/trace", nil)
	req.SetPathValue("id", "r2")
	d.handleRunTrace(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"run_id":"r2"}` {
		t.Fatalf("trace: status %d body %q", rec.Code, rec.Body.String())
	}
}
