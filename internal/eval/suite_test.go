package eval

import (
	"strings"
	"testing"
)

func TestLoadSuite_Valid(t *testing.T) {
	for _, versionLine := range []string{"version: 0.1", `version: "0.1"`} {
		suite := versionLine + `
cases:
  - name: summarize_short
    input:
      tool: respond
      args: { messages: [{ role: user, content: hi }] }
    expect:
      output_contains: ["ok"]
      max_cost_usd: 0.5
      max_duration_seconds: 60
    judge:
      enabled: true
      prompt: Score clarity.
      pass_threshold: 0.7
`
		s, err := LoadSuite([]byte(suite))
		if err != nil {
			t.Fatalf("LoadSuite(%q): %v", versionLine, err)
		}
		if string(s.Version) != "0.1" {
			t.Errorf("version = %q, want 0.1", string(s.Version))
		}
		if len(s.Cases) != 1 {
			t.Fatalf("cases = %d, want 1", len(s.Cases))
		}
		c := s.Cases[0]
		if c.Input.Tool != "respond" {
			t.Errorf("tool = %q", c.Input.Tool)
		}
		if !c.HasJudge() {
			t.Error("HasJudge = false, want true")
		}
		if c.Expect.MaxCostMicroUSD() != 500000 {
			t.Errorf("MaxCostMicroUSD = %d, want 500000", c.Expect.MaxCostMicroUSD())
		}
	}
}

func TestLoadSuite_RejectsUnknownField(t *testing.T) {
	suite := `version: 0.1
cases:
  - name: x
    input:
      tool: respond
    expect:
      output_containz: ["typo"]
`
	_, err := LoadSuite([]byte(suite))
	if err == nil || !strings.Contains(err.Error(), "output_containz") {
		t.Fatalf("expected an unknown-field error naming the typo, got %v", err)
	}
}

func TestLoadSuite_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		suite   string
		wantSub string
	}{
		{
			name:    "bad version",
			suite:   "version: 0.2\ncases:\n  - name: x\n    input:\n      tool: t\n",
			wantSub: "not supported",
		},
		{
			name:    "no cases",
			suite:   "version: 0.1\ncases: []\n",
			wantSub: "no cases",
		},
		{
			name:    "duplicate name",
			suite:   "version: 0.1\ncases:\n  - name: dup\n    input:\n      tool: t\n  - name: dup\n    input:\n      tool: t\n",
			wantSub: "more than once",
		},
		{
			name:    "missing tool",
			suite:   "version: 0.1\ncases:\n  - name: x\n    input: {}\n",
			wantSub: "no input.tool",
		},
		{
			name:    "judge without prompt",
			suite:   "version: 0.1\ncases:\n  - name: x\n    input:\n      tool: t\n    judge:\n      enabled: true\n      pass_threshold: 0.5\n",
			wantSub: "no prompt",
		},
		{
			name:    "judge threshold out of range",
			suite:   "version: 0.1\ncases:\n  - name: x\n    input:\n      tool: t\n    judge:\n      enabled: true\n      prompt: p\n      pass_threshold: 1.5\n",
			wantSub: "pass_threshold",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSuite([]byte(tc.suite))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}
