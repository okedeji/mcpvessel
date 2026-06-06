package agentfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse_Minimal(t *testing.T) {
	src := `BASE python:3.12-slim
ENTRYPOINT python3 -m agent
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Base != "python:3.12-slim" {
		t.Errorf("Base = %q, want python:3.12-slim", got.Base)
	}
	if got.Entrypoint != "python3 -m agent" {
		t.Errorf("Entrypoint = %q, want python3 -m agent", got.Entrypoint)
	}
}

func TestParse_AllDirectives(t *testing.T) {
	src := `# Comment line at the top.
BASE python:3.12-slim
ENTRYPOINT python3 -m researcher
BUILD pip install --no-cache-dir agentcage-sdk
BUILD pip install --no-cache-dir anthropic==0.34.0
MODEL anthropic/claude-3.5

ACCESS headless-browser, filesystem
USES @anthropic/web-search:1.2.0
USES PUBLIC @user/web-tool:0.5.0
BUDGET 100000
ENV LOG_LEVEL=info
SECRETS anthropic_api_key
NETWORK allow:api.example.com,docs.example.com
META description "A research agent"
META license "MIT"
EVAL ./tests/eval.yaml
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantModel := &Model{Provider: ProviderAnthropic, Name: "claude-3.5"}
	if !reflect.DeepEqual(got.Model, wantModel) {
		t.Errorf("Model = %+v, want %+v", got.Model, wantModel)
	}
	wantBuild := []string{
		"pip install --no-cache-dir agentcage-sdk",
		"pip install --no-cache-dir anthropic==0.34.0",
	}
	if !reflect.DeepEqual(got.Build, wantBuild) {
		t.Errorf("Build = %v, want %v", got.Build, wantBuild)
	}
	if !reflect.DeepEqual(got.Access, []Capability{CapHeadlessBrowser, CapFilesystem}) {
		t.Errorf("Access = %v", got.Access)
	}
	if len(got.Uses) != 2 {
		t.Fatalf("Uses len = %d, want 2", len(got.Uses))
	}
	if got.Uses[0] != (Use{Ref: "@anthropic/web-search", Version: "1.2.0"}) {
		t.Errorf("Uses[0] = %+v", got.Uses[0])
	}
	if got.Uses[1] != (Use{Ref: "@user/web-tool", Version: "0.5.0", Public: true}) {
		t.Errorf("Uses[1] = %+v", got.Uses[1])
	}
	if got.Budget != 100000 {
		t.Errorf("Budget = %d, want 100000", got.Budget)
	}
	if got.Env["LOG_LEVEL"] != "info" {
		t.Errorf("Env[LOG_LEVEL] = %q", got.Env["LOG_LEVEL"])
	}
	if !reflect.DeepEqual(got.Secrets, []string{"anthropic_api_key"}) {
		t.Errorf("Secrets = %v", got.Secrets)
	}
	if got.Network != "allow:api.example.com,docs.example.com" {
		t.Errorf("Network = %q", got.Network)
	}
	if got.Meta["description"] != "A research agent" {
		t.Errorf("Meta[description] = %q", got.Meta["description"])
	}
	if got.Meta["license"] != "MIT" {
		t.Errorf("Meta[license] = %q", got.Meta["license"])
	}
	if got.Eval != "./tests/eval.yaml" {
		t.Errorf("Eval = %q", got.Eval)
	}
}

func TestParse_CaseInsensitive(t *testing.T) {
	src := `base python:3.12-slim
EnTrYpOiNt python3 -m agent
uses public @org/x:1.0
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Base != "python:3.12-slim" {
		t.Errorf("Base = %q", got.Base)
	}
	if got.Entrypoint != "python3 -m agent" {
		t.Errorf("Entrypoint = %q", got.Entrypoint)
	}
	if len(got.Uses) != 1 || !got.Uses[0].Public {
		t.Errorf("Uses = %+v, want one public entry", got.Uses)
	}
}

func TestParse_CommentsAndBlankLines(t *testing.T) {
	src := `# leading comment
BASE python:3.12-slim

# blank line above, blank line below

ENTRYPOINT python3 -m agent
# trailing comment
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Base != "python:3.12-slim" || got.Entrypoint != "python3 -m agent" {
		t.Errorf("required fields not parsed: %+v", got)
	}
}

// Inline # is treated as part of the directive value, not as a comment.
// This matches Dockerfile and preserves legitimate uses like pip's
// `url#sha256=...` pin specs.
func TestParse_InlineHashIsNotComment(t *testing.T) {
	src := `BASE python:3.12-slim # not a comment
ENTRYPOINT python3 -m agent
BUILD pip install foo @ https://example.com/foo.tar.gz#sha256=abc
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Base != "python:3.12-slim # not a comment" {
		t.Errorf("Base = %q, want the # to be preserved", got.Base)
	}
	if len(got.Build) != 1 || got.Build[0] != "pip install foo @ https://example.com/foo.tar.gz#sha256=abc" {
		t.Errorf("Build = %v, want #sha256= preserved verbatim", got.Build)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"missing base",
			"ENTRYPOINT python3 -m agent",
			"BASE is required",
		},
		{
			"missing entrypoint",
			"BASE python:3.12-slim",
			"ENTRYPOINT is required",
		},
		{
			"unknown directive",
			"BASE x\nENTRYPOINT y\nFOOBAR baz",
			"unknown directive",
		},
		{
			"base declared twice",
			"BASE x\nBASE y\nENTRYPOINT z",
			"BASE declared twice",
		},
		{
			"entrypoint declared twice",
			"BASE x\nENTRYPOINT y\nENTRYPOINT z",
			"ENTRYPOINT declared twice",
		},
		{
			"unknown model provider",
			"BASE x\nENTRYPOINT y\nMODEL google/gemini",
			"unknown provider",
		},
		{
			"model bad form",
			"BASE x\nENTRYPOINT y\nMODEL just-a-name",
			"MODEL must be provider/model-name",
		},
		{
			"uses latest tag rejected",
			"BASE x\nENTRYPOINT y\nUSES @anthropic/web-search:latest",
			"cannot use the latest tag",
		},
		{
			"uses missing version",
			"BASE x\nENTRYPOINT y\nUSES @anthropic/web-search",
			"must include a version tag",
		},
		{
			"uses missing @",
			"BASE x\nENTRYPOINT y\nUSES anthropic/web-search:1.0",
			"must start with @",
		},
		{
			"uses missing org slash name",
			"BASE x\nENTRYPOINT y\nUSES @web-search:1.0",
			"must be @org/name:version",
		},
		{
			"unknown access",
			"BASE x\nENTRYPOINT y\nACCESS foobar",
			"unknown access capability",
		},
		{
			"budget negative",
			"BASE x\nENTRYPOINT y\nBUDGET -1",
			"must be positive",
		},
		{
			"budget extra args",
			"BASE x\nENTRYPOINT y\nBUDGET 1 USD",
			"takes a single token count",
		},
		{
			"budget bad form",
			"BASE x\nENTRYPOINT y\nBUDGET notanumber",
			"is not a token count",
		},
		{
			"reserved env prefix",
			"BASE x\nENTRYPOINT y\nENV AGENTCAGE_FOO=bar",
			"reserved AGENTCAGE_ prefix",
		},
		{
			"env bad form",
			"BASE x\nENTRYPOINT y\nENV NO_EQUALS_SIGN",
			"ENV must be KEY=VALUE",
		},
		{
			"network bad format",
			"BASE x\nENTRYPOINT y\nNETWORK something",
			"must be deny-default or allow:",
		},
		{
			"eval declared twice",
			"BASE x\nENTRYPOINT y\nEVAL a\nEVAL b",
			"EVAL declared twice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
