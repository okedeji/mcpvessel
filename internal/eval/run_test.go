package eval

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/daemon"
)

// fakeRunner returns a canned output/usage/error per tool call, so a whole
// suite runs without a daemon.
type fakeRunner struct {
	output string
	usage  daemon.RunUsage
	err    error
	calls  int
}

func (f *fakeRunner) RunOnceUsage(_ context.Context, _ daemon.RunRequest, _ io.Writer) (string, daemon.RunUsage, error) {
	f.calls++
	return f.output, f.usage, f.err
}

type fakeScorer struct {
	verdict Verdict
	err     error
}

func (f fakeScorer) Score(_ context.Context, _, _, _ string) (Verdict, error) {
	return f.verdict, f.err
}

func manifestWith(main string, expose ...string) *bundle.Manifest {
	return &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Main: main, Expose: expose}}
}

func suiteWith(cases ...Case) *Suite {
	return &Suite{Version: "0.1", Cases: cases}
}

func TestRun_AllPass(t *testing.T) {
	d := &fakeRunner{output: "the answer is ok", usage: daemon.RunUsage{CostMicroUSD: 31000, CallDuration: 12 * time.Second}}
	suite := suiteWith(Case{
		Name:   "c1",
		Input:  Input{Tool: "respond"},
		Expect: Expect{OutputContains: []string{"ok"}, MaxCostUSD: 0.1, MaxDurationSeconds: 60},
	})
	report, err := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed != 1 || report.Failed != 0 {
		t.Errorf("passed=%d failed=%d, want 1/0", report.Passed, report.Failed)
	}
	if report.CostMicroUSD != 31000 {
		t.Errorf("cost = %d, want 31000", report.CostMicroUSD)
	}
}

func TestRun_ContainsFailureReason(t *testing.T) {
	d := &fakeRunner{output: "nope"}
	suite := suiteWith(Case{Name: "c1", Input: Input{Tool: "respond"}, Expect: Expect{OutputContains: []string{"cannot share"}}})
	report, _ := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if report.Failed != 1 {
		t.Fatalf("failed = %d, want 1", report.Failed)
	}
	if got := strings.Join(report.Cases[0].Failures, ";"); !strings.Contains(got, `does not contain "cannot share"`) {
		t.Errorf("failures = %q", got)
	}
}

func TestRun_RunErrorIsCaseFailureNotAbort(t *testing.T) {
	d := &fakeRunner{err: errors.New("cage crashed"), usage: daemon.RunUsage{CostMicroUSD: 500}}
	suite := suiteWith(
		Case{Name: "c1", Input: Input{Tool: "respond"}},
		Case{Name: "c2", Input: Input{Tool: "respond"}, Expect: Expect{OutputContains: []string{"x"}}},
	)
	report, err := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if err != nil {
		t.Fatalf("Run returned an error instead of failing the case: %v", err)
	}
	if d.calls != 2 {
		t.Errorf("ran %d cases, want the suite to continue past the error (2)", d.calls)
	}
	if report.Failed != 2 {
		t.Errorf("failed = %d, want 2", report.Failed)
	}
	if !strings.Contains(report.Cases[0].Failures[0], "run failed") {
		t.Errorf("failure = %q, want a run-failed reason", report.Cases[0].Failures[0])
	}
	if report.Cases[0].CostMicroUSD != 500 {
		t.Errorf("a failed run should still report its spend, got %d", report.Cases[0].CostMicroUSD)
	}
}

func TestRun_TimeoutReasonTranslated(t *testing.T) {
	d := &fakeRunner{err: errors.New("forwarding tools/call: context deadline exceeded")}
	suite := suiteWith(Case{Name: "c1", Input: Input{Tool: "respond"}, Expect: Expect{MaxDurationSeconds: 30}})
	report, _ := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if got := report.Cases[0].Failures[0]; !strings.Contains(got, "exceeded max_duration_seconds (30s)") {
		t.Errorf("failure = %q, want the translated timeout reason", got)
	}
}

func TestRun_CostCeilingPostChecked(t *testing.T) {
	// The gateway budget is soft, so a run can complete over budget; the post
	// check catches it even when the run itself did not error.
	d := &fakeRunner{output: "ok", usage: daemon.RunUsage{CostMicroUSD: 120000}}
	suite := suiteWith(Case{Name: "c1", Input: Input{Tool: "respond"}, Expect: Expect{MaxCostUSD: 0.1}})
	report, _ := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if report.Failed != 1 || !strings.Contains(report.Cases[0].Failures[0], "exceeds max_cost_usd") {
		t.Errorf("expected a cost-ceiling failure, got %+v", report.Cases[0])
	}
}

func TestRun_JudgeMeanOverJudgedCasesOnly(t *testing.T) {
	d := &fakeRunner{output: "answer"}
	j := fakeScorer{verdict: Verdict{Score: 0.8, Reason: "clear", CostMicroUSD: 200}}
	suite := suiteWith(
		Case{Name: "judged", Input: Input{Tool: "respond"}, Judge: &Judge{Enabled: true, Prompt: "grade", PassThreshold: 0.7}},
		Case{Name: "plain", Input: Input{Tool: "respond"}},
	)
	report, err := Run(context.Background(), d, j, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.JudgeCount != 1 {
		t.Errorf("JudgeCount = %d, want 1", report.JudgeCount)
	}
	if report.JudgeScore == nil || *report.JudgeScore != 0.8 {
		t.Errorf("JudgeScore = %v, want 0.8 (mean over the one judged case)", report.JudgeScore)
	}
	if report.JudgeCostMicroUSD != 200 {
		t.Errorf("JudgeCostMicroUSD = %d, want 200", report.JudgeCostMicroUSD)
	}
	// Judge cost is not folded into the case's own cost.
	if report.Cases[0].CostMicroUSD != 0 {
		t.Errorf("case cost = %d, judge spend must not leak into it", report.Cases[0].CostMicroUSD)
	}
}

func TestRun_JudgeBelowThresholdFails(t *testing.T) {
	d := &fakeRunner{output: "answer"}
	j := fakeScorer{verdict: Verdict{Score: 0.4, Reason: "vague"}}
	suite := suiteWith(Case{Name: "judged", Input: Input{Tool: "respond"}, Judge: &Judge{Enabled: true, Prompt: "grade", PassThreshold: 0.7}})
	report, _ := Run(context.Background(), d, j, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if report.Failed != 1 || !strings.Contains(report.Cases[0].Failures[0], "below threshold") {
		t.Errorf("expected a below-threshold failure, got %+v", report.Cases[0])
	}
}

func TestRun_JudgeNeededButNoneConfigured(t *testing.T) {
	d := &fakeRunner{output: "answer"}
	suite := suiteWith(Case{Name: "judged", Input: Input{Tool: "respond"}, Judge: &Judge{Enabled: true, Prompt: "grade", PassThreshold: 0.7}})
	_, err := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite})
	if err == nil || !strings.Contains(err.Error(), "needs an LLM judge") {
		t.Fatalf("err = %v, want a missing-judge harness error", err)
	}
}

func TestRun_UnknownCaseFilter(t *testing.T) {
	d := &fakeRunner{output: "ok"}
	suite := suiteWith(Case{Name: "real", Input: Input{Tool: "respond"}})
	_, err := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond"), Suite: suite, CaseName: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "no case named") {
		t.Fatalf("err = %v, want an unknown-case error", err)
	}
}

func TestRun_ToolNotPublicFailsCaseWithoutBooting(t *testing.T) {
	d := &fakeRunner{output: "ok"}
	suite := suiteWith(Case{Name: "c1", Input: Input{Tool: "secret_tool"}})
	report, _ := Run(context.Background(), d, nil, Options{Ref: "x", Manifest: manifestWith("respond", "fetch"), Suite: suite})
	if report.Failed != 1 || !strings.Contains(report.Cases[0].Failures[0], "not public") {
		t.Errorf("expected a not-public failure, got %+v", report.Cases[0])
	}
	if d.calls != 0 {
		t.Errorf("runner was called %d times; a non-public tool must not boot a cage", d.calls)
	}
}
