package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/okedeji/agentcage/internal/llmgateway"
	"github.com/okedeji/agentcage/internal/mcpgateway"
	"github.com/okedeji/agentcage/internal/replay"
)

func TestReplayEvents_MapsRecordsInOrder(t *testing.T) {
	records := []llmgateway.CallRecord{
		{Agent: "root", Request: []byte(`{"q":1}`), Response: []byte(`{"a":2}`), PromptTokens: 10, CompletionTokens: 5, CostMicroUSD: 100},
		{Agent: "root", Response: []byte("data: chunk\n\n"), Streamed: true},
	}
	events := replayEvents(records, nil)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Seq != 0 || events[0].Type != replay.EventLLMComplete {
		t.Errorf("first event = %+v", events[0])
	}
	if string(events[0].Request) != `{"q":1}` {
		t.Errorf("JSON request not embedded raw: %s", events[0].Request)
	}
	if events[1].Type != replay.EventLLMStream {
		t.Errorf("streamed record should be llm.stream: %+v", events[1])
	}
	// A non-JSON streamed body embeds as a JSON string, keeping the artifact valid.
	var s string
	if err := json.Unmarshal(events[1].Response, &s); err != nil || s != "data: chunk\n\n" {
		t.Errorf("streamed response = %s, want a JSON string", events[1].Response)
	}
}

func TestReplayEvents_MergesLLMAndSubagentByStart(t *testing.T) {
	records := []llmgateway.CallRecord{{StartUnixNano: 200, Response: []byte(`{}`)}}
	subRecords := []mcpgateway.SubCallRecord{{Edge: "web", Tool: "search", StartUnixNano: 100, Args: []byte(`{"q":1}`), Response: []byte(`{}`)}}

	events := replayEvents(records, subRecords)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	// The earlier sub-agent call comes first and seqs start at 0.
	if events[0].Type != "subagent.web.search" || events[0].Seq != 0 {
		t.Errorf("first event = %+v, want the t=100 sub-agent call at seq 0", events[0])
	}
	if events[1].Type != replay.EventLLMComplete || events[1].Seq != 1 {
		t.Errorf("second event = %+v, want the t=200 llm call at seq 1", events[1])
	}
}

func TestHandleRunReplay(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	d := New()

	// No artifact is a 404.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runs/nope/replay", nil)
	req.SetPathValue("id", "nope")
	d.handleRunReplay(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing artifact: status %d, want 404", rec.Code)
	}

	// A written artifact reads back verbatim.
	path, _ := replay.Path("echo-1")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"run_id":"echo-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/runs/echo-1/replay", nil)
	req.SetPathValue("id", "echo-1")
	d.handleRunReplay(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"run_id":"echo-1"}` {
		t.Fatalf("artifact: status %d body %q", rec.Code, rec.Body.String())
	}
}
