package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/eval"
)

func TestPrintCaseResult_Pass(t *testing.T) {
	var buf bytes.Buffer
	printCaseResult(&buf, eval.CaseResult{Name: "summarize_short", Passed: true, CostMicroUSD: 31000, DurationMS: 12400})
	got := buf.String()
	if !strings.Contains(got, "--- PASS: summarize_short") {
		t.Errorf("missing PASS line: %q", got)
	}
	if !strings.Contains(got, "12.4s") || !strings.Contains(got, "$0.031") {
		t.Errorf("missing duration/cost: %q", got)
	}
}

func TestPrintCaseResult_FailWithReasons(t *testing.T) {
	var buf bytes.Buffer
	printCaseResult(&buf, eval.CaseResult{
		Name:         "refuses_pii",
		Passed:       false,
		DurationMS:   8100,
		CostMicroUSD: 20000,
		Failures:     []string{`output does not contain "cannot share"`, "judge score 0.40 is below threshold 0.70"},
	})
	got := buf.String()
	if !strings.Contains(got, "--- FAIL: refuses_pii") {
		t.Errorf("missing FAIL line: %q", got)
	}
	for _, reason := range []string{`output does not contain "cannot share"`, "judge score 0.40 is below threshold 0.70"} {
		if !strings.Contains(got, "    "+reason) {
			t.Errorf("failure reason %q not indented in:\n%s", reason, got)
		}
	}
}

func TestPrintSummary_AllPassWithJudge(t *testing.T) {
	var buf bytes.Buffer
	score := 0.81
	printSummary(&buf, "@okedeji/researcher:0.1", &eval.Report{
		Passed: 3, Failed: 0, JudgeScore: &score, JudgeCount: 2,
		CostMicroUSD: 81000, JudgeCostMicroUSD: 2000, ElapsedMS: 44000,
	})
	got := buf.String()
	if !strings.HasPrefix(got, "ok\t") {
		t.Errorf("summary should start with ok: %q", got)
	}
	for _, want := range []string{"3 passed", "judge 0.81 (2 scored, $0.002)", "$0.081 in 44s"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestPrintSummary_FailNoJudge(t *testing.T) {
	var buf bytes.Buffer
	printSummary(&buf, "echo.agent", &eval.Report{Passed: 1, Failed: 1, CostMicroUSD: 51000, ElapsedMS: 20500})
	got := buf.String()
	if !strings.HasPrefix(got, "FAIL\t") {
		t.Errorf("summary should start with FAIL: %q", got)
	}
	if !strings.Contains(got, "1 passed, 1 failed") {
		t.Errorf("missing counts: %q", got)
	}
	if strings.Contains(got, "judge") {
		t.Errorf("no case was judged, summary should omit the judge segment: %q", got)
	}
}

func TestSuiteNeedsJudge(t *testing.T) {
	suite := &eval.Suite{Version: "0.1", Cases: []eval.Case{
		{Name: "plain", Input: eval.Input{Tool: "t"}},
		{Name: "judged", Input: eval.Input{Tool: "t"}, Judge: &eval.Judge{Enabled: true, Prompt: "p", PassThreshold: 0.5}},
	}}
	if !suiteNeedsJudge(suite, "") {
		t.Error("full suite has a judged case, want true")
	}
	if suiteNeedsJudge(suite, "plain") {
		t.Error("the selected case is not judged, want false")
	}
	if !suiteNeedsJudge(suite, "judged") {
		t.Error("the selected case is judged, want true")
	}
}
