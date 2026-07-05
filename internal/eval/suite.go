// Package eval runs an agent's eval suite: it loads the YAML the EVAL directive
// points at, runs each case in a real cage, checks the output against the case's
// expectations, and reports pass/fail counts plus an aggregate judge score.
package eval

import (
	"bytes"
	"fmt"
	"math"
	"os"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the locked eval-suite schema version. Only "0.1" is valid.
const SchemaVersion = "0.1"

// Suite is one agent's eval suite, the parsed form of the file the EVAL
// directive points at. The schema is locked small on purpose (DESIGN.md 5);
// authors grow into it.
type Suite struct {
	Version version `yaml:"version"`
	Cases   []Case  `yaml:"cases"`
}

// Case is one eval: an input to send the agent and the expectations its output
// must satisfy. A case passes iff every declared expectation passes; an
// undeclared expectation is not checked.
type Case struct {
	Name   string `yaml:"name"`
	Input  Input  `yaml:"input"`
	Expect Expect `yaml:"expect"`
	Judge  *Judge `yaml:"judge,omitempty"`
}

// Input names the exposed tool to invoke and the arguments to send it. Tool
// must be the agent's MAIN or one of its EXPOSE'd tools.
type Input struct {
	Tool string         `yaml:"tool"`
	Args map[string]any `yaml:"args,omitempty"`
}

// Expect is the set of checks a case's output must pass. Every field is
// optional; an unset field is not checked.
type Expect struct {
	OutputContains     []string `yaml:"output_contains,omitempty"`
	OutputNotContains  []string `yaml:"output_not_contains,omitempty"`
	MaxCostUSD         float64  `yaml:"max_cost_usd,omitempty"`
	MaxDurationSeconds int      `yaml:"max_duration_seconds,omitempty"`
}

// Judge is the optional LLM-as-judge check: a rubric an operator-configured
// model scores the output against, passing when the score meets the threshold.
type Judge struct {
	Enabled       bool    `yaml:"enabled"`
	Prompt        string  `yaml:"prompt"`
	PassThreshold float64 `yaml:"pass_threshold"`
}

// version reads the schema version verbatim so a bare `version: 0.1` (a YAML
// float) and a quoted `version: "0.1"` both land as the string "0.1" instead of
// a lossy float like 0.1 rendering as "0.1" only by luck.
type version string

func (v *version) UnmarshalYAML(node *yaml.Node) error {
	*v = version(node.Value)
	return nil
}

// LoadSuiteFile reads and validates an eval suite from a path.
func LoadSuiteFile(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading eval suite %s: %w", path, err)
	}
	return LoadSuite(data)
}

// LoadSuite parses and validates an eval suite from YAML bytes. Unknown fields
// are rejected: a typo like output_containz must fail loading, not silently
// leave an expectation unchecked and report a false pass.
func LoadSuite(data []byte) (*Suite, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Suite
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parsing eval suite: %w", err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Suite) validate() error {
	if string(s.Version) != SchemaVersion {
		return fmt.Errorf("eval suite version %q is not supported (want %q)", string(s.Version), SchemaVersion)
	}
	if len(s.Cases) == 0 {
		return fmt.Errorf("eval suite has no cases")
	}
	seen := make(map[string]bool, len(s.Cases))
	for i := range s.Cases {
		c := &s.Cases[i]
		if c.Name == "" {
			return fmt.Errorf("case %d has no name", i)
		}
		if seen[c.Name] {
			return fmt.Errorf("case %q is declared more than once", c.Name)
		}
		seen[c.Name] = true
		if c.Input.Tool == "" {
			return fmt.Errorf("case %q has no input.tool", c.Name)
		}
		if c.Expect.MaxCostUSD < 0 {
			return fmt.Errorf("case %q max_cost_usd is negative", c.Name)
		}
		if c.Expect.MaxDurationSeconds < 0 {
			return fmt.Errorf("case %q max_duration_seconds is negative", c.Name)
		}
		if c.Judge != nil && c.Judge.Enabled {
			if c.Judge.Prompt == "" {
				return fmt.Errorf("case %q enables the judge but has no prompt", c.Name)
			}
			if c.Judge.PassThreshold <= 0 || c.Judge.PassThreshold > 1 {
				return fmt.Errorf("case %q judge pass_threshold %v is not in (0, 1]", c.Name, c.Judge.PassThreshold)
			}
		}
	}
	return nil
}

// MaxCostMicroUSD returns the case's cost ceiling in micro-USD, the scale the
// gateway budget and history record use, or 0 when the case sets none.
func (e Expect) MaxCostMicroUSD() int64 {
	return int64(math.Round(e.MaxCostUSD * 1e6))
}

// HasJudge reports whether the case runs an LLM judge.
func (c Case) HasJudge() bool {
	return c.Judge != nil && c.Judge.Enabled
}
