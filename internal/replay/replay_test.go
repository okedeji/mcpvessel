package replay

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRawOrString(t *testing.T) {
	if got := RawOrString([]byte(`{"a":1}`)); string(got) != `{"a":1}` {
		t.Errorf("valid JSON = %s, want passthrough", got)
	}
	// A non-JSON body (streamed SSE) must become a JSON string.
	got := RawOrString([]byte("data: hi\n\n"))
	var s string
	if err := json.Unmarshal(got, &s); err != nil || s != "data: hi\n\n" {
		t.Errorf("non-JSON = %s (err %v), want a JSON string of the body", got, err)
	}
	if RawOrString(nil) != nil {
		t.Error("empty payload should produce nil, not an empty value")
	}
}

func TestWriteAndPath(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	rec := &Recording{
		Version:  Version,
		AgentRef: "@me/echo:1",
		RunID:    "echo-1",
		Input:    Input{Tool: "main"},
		Events:   []Event{{Seq: 0, Type: EventLLMComplete, Request: RawOrString([]byte(`{"q":1}`))}},
		Result:   Result{Output: "hi", Status: "succeeded"},
	}
	if err := Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	path, err := Path("echo-1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	var got Recording
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("artifact is not valid JSON: %v", err)
	}
	if got.RunID != "echo-1" || len(got.Events) != 1 || got.Result.Output != "hi" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}
