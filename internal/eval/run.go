package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/daemon"
)

// nowFunc is overridable so tests can pin the suite's elapsed time.
var nowFunc = time.Now

// runner is the daemon seam: the eval runner sends each case through the same
// one-shot path as `agentcage run`. It is defined here, at the point of
// consumption, so a test drives a whole suite with a fake and no daemon.
type runner interface {
	RunOnceUsage(ctx context.Context, req daemon.RunRequest, logs io.Writer) (string, daemon.RunUsage, error)
}

// scorer grades one case's output against a rubric. It is nil unless the suite
// has a judged case.
type scorer interface {
	Score(ctx context.Context, rubric, input, output string) (Verdict, error)
}

// Verdict is a judge's grade of one output: a score in [0, 1], a one-line
// reason, and what the grading call itself cost.
type Verdict struct {
	Score        float64
	Reason       string
	CostMicroUSD int64
}

// Options configures a suite run.
type Options struct {
	Ref      string           // what the daemon resolves: a ref, a content hash, or a bundle path
	Manifest *bundle.Manifest // the resolved bundle's manifest, for the public-tool check
	Suite    *Suite
	CaseName string            // run only this case; empty runs the whole suite
	Env      map[string]string // operator ENV inputs, passed to every case
	Secrets  map[string]string // operator secrets, passed to every case
	Logs     io.Writer         // build progress and agent stderr, streamed live
	OnStart  func(name string) // called as each case starts, for live progress
	OnCase   func(CaseResult)  // called as each case finishes
}

// Run executes a suite (or one case of it) and returns the report. It errors
// only on a harness fault the operator must fix before anything runs: an
// unknown --case, or a judged case with no judge configured. A case that
// fails its expectations, errors, times out, or overspends is a failed case in
// the report, not a returned error; the suite always runs to completion.
func Run(ctx context.Context, d runner, j scorer, opts Options) (*Report, error) {
	cases := opts.Suite.Cases
	if opts.CaseName != "" {
		one, ok := findCase(opts.Suite, opts.CaseName)
		if !ok {
			return nil, fmt.Errorf("no case named %q (cases: %s)", opts.CaseName, strings.Join(caseNames(opts.Suite), ", "))
		}
		cases = []Case{one}
	}
	for _, c := range cases {
		if c.HasJudge() && j == nil {
			return nil, fmt.Errorf("case %q needs an LLM judge but none is configured", c.Name)
		}
	}

	report := &Report{}
	start := nowFunc()
	var judgeSum float64
	for _, c := range cases {
		if opts.OnStart != nil {
			opts.OnStart(c.Name)
		}
		res := runCase(ctx, d, j, opts, c)
		report.Cases = append(report.Cases, res)
		if res.Passed {
			report.Passed++
		} else {
			report.Failed++
		}
		report.CostMicroUSD += res.CostMicroUSD
		report.JudgeCostMicroUSD += res.judgeCostMicroUSD
		if res.JudgeScore != nil {
			judgeSum += *res.JudgeScore
			report.JudgeCount++
		}
		if opts.OnCase != nil {
			opts.OnCase(res)
		}
	}
	report.ElapsedMS = nowFunc().Sub(start).Milliseconds()
	if report.JudgeCount > 0 {
		mean := judgeSum / float64(report.JudgeCount)
		report.JudgeScore = &mean
	}
	return report, nil
}

func runCase(ctx context.Context, d runner, j scorer, opts Options, c Case) CaseResult {
	res := CaseResult{Name: c.Name}

	// The public-tool check happens before any boot: a case that names a private
	// or nonexistent tool fails cheaply, with the same contract call/run enforce.
	if err := toolIsPublic(opts.Manifest, c.Input.Tool); err != nil {
		res.Failures = []string{err.Error()}
		return res
	}

	output, usage, err := d.RunOnceUsage(ctx, daemon.RunRequest{
		Ref:            opts.Ref,
		Tool:           c.Input.Tool,
		Args:           c.Input.Args,
		Env:            opts.Env,
		Secrets:        opts.Secrets,
		Budget:         c.Expect.MaxCostMicroUSD(),
		TimeoutSeconds: int64(c.Expect.MaxDurationSeconds),
	}, opts.Logs)
	res.CostMicroUSD = usage.CostMicroUSD
	res.DurationMS = usage.CallDuration.Milliseconds()

	// A run error (a crash, an over-budget refusal, or the timeout firing) is a
	// case failure. There is no output to check or judge, so the case ends here.
	if err != nil {
		res.Failures = []string{runFailureReason(err, c)}
		return res
	}

	res.Failures = append(res.Failures, checkExpectations(c.Expect, output, usage)...)

	if c.HasJudge() {
		verdict, jerr := j.Score(ctx, c.Judge.Prompt, describeInput(c), output)
		if jerr != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("judge error: %v", jerr))
		} else {
			res.judgeCostMicroUSD = verdict.CostMicroUSD
			score := verdict.Score
			res.JudgeScore = &score
			res.JudgeReason = verdict.Reason
			if verdict.Score < c.Judge.PassThreshold {
				res.Failures = append(res.Failures, fmt.Sprintf("judge score %.2f is below threshold %.2f", verdict.Score, c.Judge.PassThreshold))
			}
		}
	}

	res.Passed = len(res.Failures) == 0
	return res
}

// runFailureReason phrases a run error for the report. A fired timeout reads as
// the ceiling it broke, not the raw context error the daemon surfaced.
func runFailureReason(err error, c Case) string {
	if c.Expect.MaxDurationSeconds > 0 && strings.Contains(err.Error(), "context deadline exceeded") {
		return fmt.Sprintf("exceeded max_duration_seconds (%ds)", c.Expect.MaxDurationSeconds)
	}
	return fmt.Sprintf("run failed: %v", err)
}

func checkExpectations(e Expect, output string, usage daemon.RunUsage) []string {
	var fails []string
	for _, want := range e.OutputContains {
		if !strings.Contains(output, want) {
			fails = append(fails, fmt.Sprintf("output does not contain %q", want))
		}
	}
	for _, no := range e.OutputNotContains {
		if strings.Contains(output, no) {
			fails = append(fails, fmt.Sprintf("output contains forbidden %q", no))
		}
	}
	// The gateway budget is a soft cap: a call already in flight can push spend
	// past it and still complete, so a passing run is post-checked against the
	// recorded cost too.
	if e.MaxCostUSD > 0 && usage.CostMicroUSD > e.MaxCostMicroUSD() {
		fails = append(fails, fmt.Sprintf("cost %s exceeds max_cost_usd %s", FormatUSD(usage.CostMicroUSD), FormatUSD(e.MaxCostMicroUSD())))
	}
	if e.MaxDurationSeconds > 0 && usage.CallDuration > time.Duration(e.MaxDurationSeconds)*time.Second {
		fails = append(fails, fmt.Sprintf("duration %s exceeds max_duration_seconds (%ds)", usage.CallDuration.Round(time.Millisecond), e.MaxDurationSeconds))
	}
	return fails
}

// toolIsPublic mirrors the MAIN-or-EXPOSE contract call and run enforce: an
// eval case may only invoke a tool the agent exposes.
func toolIsPublic(m *bundle.Manifest, tool string) error {
	if m.Agentfile.Main == tool {
		return nil
	}
	for _, name := range m.Agentfile.Expose {
		if name == tool {
			return nil
		}
	}
	return fmt.Errorf("tool %q is not public on this bundle (declare it via MAIN or EXPOSE)", tool)
}

// describeInput renders a case's input for the judge so the model sees what was
// asked, not just the answer.
func describeInput(c Case) string {
	if len(c.Input.Args) == 0 {
		return c.Input.Tool
	}
	b, err := json.Marshal(c.Input.Args)
	if err != nil {
		return c.Input.Tool
	}
	return c.Input.Tool + " " + string(b)
}

func findCase(s *Suite, name string) (Case, bool) {
	for _, c := range s.Cases {
		if c.Name == name {
			return c, true
		}
	}
	return Case{}, false
}

func caseNames(s *Suite) []string {
	names := make([]string, len(s.Cases))
	for i, c := range s.Cases {
		names[i] = c.Name
	}
	return names
}
