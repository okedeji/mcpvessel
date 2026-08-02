package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/runtime"
)

// A single MAIN-less collection collapses to one full-URL endpoint, and the
// REST section names the agent and skips the prompt endpoint it does not have.
func TestPrintServeReport_SingleToolCollection(t *testing.T) {
	res := daemon.ServeResult{
		Listen: "127.0.0.1:7000",
		Flat:   daemon.ServedFlat{Path: "/mcp", Tools: []string{"docs_search_docs", "docs_search_code"}},
		Agents: []daemon.ServedAgent{{Address: "docs", Tools: []string{"search_docs", "search_code"}}},
	}
	policies := map[string]exposedPolicy{
		"docs": {Egress: "allow:api.github.com", Secrets: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}, Optional: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}},
	}
	var buf bytes.Buffer
	printServeReport(&buf, res, policies, nil, runtime.ScopedSecrets{}, false)
	got := buf.String()

	want := `Serving docs on http://127.0.0.1:7000

MCP endpoint, point your client here:
  http://127.0.0.1:7000/mcp
  2 tools: docs_search_docs, docs_search_code

Egress:
  docs: api.github.com (from bundle)
Secrets:
  docs: GITHUB_PERSONAL_ACCESS_TOKEN (optional, not granted)

REST on the same port:
  POST http://127.0.0.1:7000/agents/docs/tools/<tool>  JSON args in, JSON result out
`
	if got != want {
		t.Errorf("report:\n%s\nwant:\n%s", got, want)
	}
}

// Several agents keep the merged /mcp plus one endpoint each, and a single
// MAIN-bearing agent gets the prompt endpoint advertised by name.
func TestPrintServeReport_MultiAgentWithMain(t *testing.T) {
	res := daemon.ServeResult{
		Listen: "127.0.0.1:7000",
		Flat:   daemon.ServedFlat{Path: "/mcp", Tools: []string{"oncall_triage", "time_now"}},
		Agents: []daemon.ServedAgent{
			{Address: "oncall", Tools: []string{"triage"}, Main: "triage"},
			{Address: "time", Tools: []string{"now"}},
		},
	}
	policies := map[string]exposedPolicy{"oncall": {}, "time": {}}
	var buf bytes.Buffer
	printServeReport(&buf, res, policies, nil, runtime.ScopedSecrets{}, false)
	got := buf.String()

	for _, line := range []string{
		"Serving 2 agents on http://127.0.0.1:7000",
		"MCP endpoints, one URL for your MCP client:",
		"  http://127.0.0.1:7000/mcp  (all public tools)  2 tools: oncall_triage, time_now",
		"  http://127.0.0.1:7000/agents/oncall/mcp  1 tool: triage",
		"  http://127.0.0.1:7000/agents/time/mcp  1 tool: now",
		"  POST http://127.0.0.1:7000/agents/<name>/tools/<tool>  JSON args in, JSON result out",
		"  POST http://127.0.0.1:7000/agents/oncall  prompt it with {\"prompt\": ...}; add {\"stream\": true} for SSE chunks",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("report missing %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "agent(s)") {
		t.Errorf("report kept the lazy plural:\n%s", got)
	}
}

// A tool list past eight names is capped with a count and an ellipsis so one
// well-stocked server cannot wrap the report off the terminal.
func TestToolSummary_CapsLongLists(t *testing.T) {
	var names []string
	for i := range 12 {
		names = append(names, fmt.Sprintf("tool_%d", i))
	}
	got := toolSummary(names)
	want := "12 tools: tool_0, tool_1, tool_2, tool_3, tool_4, tool_5, tool_6, tool_7, ..."
	if got != want {
		t.Errorf("toolSummary = %q, want %q", got, want)
	}
	if got := toolSummary([]string{"solo"}); got != "1 tool: solo" {
		t.Errorf("toolSummary singular = %q, want %q", got, "1 tool: solo")
	}
}

// 'serve add' reports on a door that already holds agents this process knows
// nothing about. Rendering their missing policy as "none preset" / "none
// declared" told the operator a running cage had no allowed hosts and held no
// secrets, when it may have had both. Understating a cage's reach is the
// dangerous direction to be wrong in, so an unknown agent says so.
func TestPrintServeReport_DoesNotInventPolicyForAgentsAlreadyServing(t *testing.T) {
	res := daemon.ServeResult{
		Listen: "127.0.0.1:8080",
		Flat:   daemon.ServedFlat{Path: "/mcp", Tools: []string{"notes_save_note", "weather_get_forecast"}},
		Agents: []daemon.ServedAgent{
			{Address: "notes", Tools: []string{"save_note"}},      // already on the door
			{Address: "weather", Tools: []string{"get_forecast"}}, // added by this command
		},
	}
	policies := map[string]exposedPolicy{"weather": {}}
	var buf bytes.Buffer
	printServeReport(&buf, res, policies, nil, runtime.ScopedSecrets{}, false)
	got := buf.String()

	if strings.Count(got, "notes: "+unchangedByThisCommand) != 2 {
		t.Errorf("want notes marked untouched under both Egress and Secrets:\n%s", got)
	}
	// The agent this command did serve is still reported in full.
	if !strings.Contains(got, "weather: none preset") {
		t.Errorf("want the newly served agent's egress reported:\n%s", got)
	}
	if !strings.Contains(got, "weather: none declared") {
		t.Errorf("want the newly served agent's secrets reported:\n%s", got)
	}
	// The claim that caused the bug must not appear against the untouched agent.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "notes:") && strings.Contains(line, "none") {
			t.Errorf("asserted an absence for an agent whose policy was never loaded: %q", line)
		}
	}
}
