package agentfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse_Minimal(t *testing.T) {
	src := `FROM python:3.12-slim
ENTRYPOINT python3 -m agent
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.From != "python:3.12-slim" {
		t.Errorf("From = %q, want python:3.12-slim", got.From)
	}
	if got.Entrypoint != "python3 -m agent" {
		t.Errorf("Entrypoint = %q, want python3 -m agent", got.Entrypoint)
	}
}

func TestParse_AllDirectives(t *testing.T) {
	src := `# Comment line at the top.
FROM python:3.12-slim
RUN pip install --no-cache-dir mcp
RUN pip install --no-cache-dir anthropic==0.34.0
MODEL anthropic/claude-3.5

USES @anthropic/web-search:1.2.0
USES PUBLIC @user/web-tool:0.5.0
BUDGET 5.00
RESOURCES cpu=2 mem=2g pids=1024
ENV LOG_LEVEL=info
ENV SYSTEM_PROMPT
SECRETS anthropic_api_key
EGRESS allow:api.example.com,docs.example.com
META description "A research agent"
META license "MIT"
EVAL ./tests/eval.yaml
ENTRYPOINT python3 -m researcher
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantModel := &Model{Provider: "anthropic", Name: "claude-3.5"}
	if !reflect.DeepEqual(got.Model, wantModel) {
		t.Errorf("Model = %+v, want %+v", got.Model, wantModel)
	}
	wantRun := []string{
		"pip install --no-cache-dir mcp",
		"pip install --no-cache-dir anthropic==0.34.0",
	}
	if !reflect.DeepEqual(got.Run, wantRun) {
		t.Errorf("Run = %v, want %v", got.Run, wantRun)
	}
	if len(got.Uses) != 2 {
		t.Fatalf("Uses len = %d, want 2", len(got.Uses))
	}
	if !reflect.DeepEqual(got.Uses[0], Use{Ref: "@anthropic/web-search", Version: "1.2.0"}) {
		t.Errorf("Uses[0] = %+v", got.Uses[0])
	}
	if !reflect.DeepEqual(got.Uses[1], Use{Ref: "@user/web-tool", Version: "0.5.0", Public: true}) {
		t.Errorf("Uses[1] = %+v", got.Uses[1])
	}
	if got.Budget != 5_000_000 {
		t.Errorf("Budget = %d, want 5000000 micro-USD", got.Budget)
	}
	wantResources := &Resources{CPUs: "2", Mem: "2g", Pids: 1024}
	if !reflect.DeepEqual(got.Resources, wantResources) {
		t.Errorf("Resources = %+v, want %+v", got.Resources, wantResources)
	}
	if got.Env["LOG_LEVEL"] != "info" {
		t.Errorf("Env[LOG_LEVEL] = %q", got.Env["LOG_LEVEL"])
	}
	// Value-less ENV: declared, no default.
	if v, ok := got.Env["SYSTEM_PROMPT"]; !ok || v != "" {
		t.Errorf("Env[SYSTEM_PROMPT] = %q, ok=%v; want declared with empty default", v, ok)
	}
	if !reflect.DeepEqual(got.Secrets, []string{"anthropic_api_key"}) {
		t.Errorf("Secrets = %v", got.Secrets)
	}
	if got.Egress != "allow:api.example.com,docs.example.com" {
		t.Errorf("Egress = %q", got.Egress)
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

func TestParse_MainAndExpose(t *testing.T) {
	src := `FROM python:3.12-slim
ENTRYPOINT python3 -m agent
MODEL anthropic/claude-3.5
MAIN respond
EXPOSE fetch_paper
EXPOSE cite_count, parse_doi
EXPOSE cite_count
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Main != "respond" {
		t.Errorf("Main = %q, want %q", got.Main, "respond")
	}
	wantExpose := []string{"fetch_paper", "cite_count", "parse_doi"}
	if !reflect.DeepEqual(got.Expose, wantExpose) {
		t.Errorf("Expose = %v, want %v (duplicates should be deduped, order preserved)", got.Expose, wantExpose)
	}
}

func TestParse_MainOptional(t *testing.T) {
	src := `FROM node:20-slim
ENTRYPOINT node dist/server.js
EXPOSE search
EXPOSE news
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Main != "" {
		t.Errorf("Main = %q, want empty for tool collection", got.Main)
	}
	if !reflect.DeepEqual(got.Expose, []string{"search", "news"}) {
		t.Errorf("Expose = %v", got.Expose)
	}
}

func TestParse_UsesDeny(t *testing.T) {
	src := `FROM python:3.12-slim
ENTRYPOINT python3 -m agent
USES @anthropic/web-search:1.2.0 DENY deep_crawl
USES PUBLIC @user/billing:0.5.0 DENY charge_card,refund
USES @vendor/safe:1.0.0 DENY a, b, c
USES @vendor/other:1.0.0 DENY a b c
USES @vendor/dedup:1.0.0 DENY a,a,b
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Uses) != 5 {
		t.Fatalf("Uses len = %d, want 5", len(got.Uses))
	}
	want := []Use{
		{Ref: "@anthropic/web-search", Version: "1.2.0", Deny: []string{"deep_crawl"}},
		{Ref: "@user/billing", Version: "0.5.0", Public: true, Deny: []string{"charge_card", "refund"}},
		{Ref: "@vendor/safe", Version: "1.0.0", Deny: []string{"a", "b", "c"}},
		{Ref: "@vendor/other", Version: "1.0.0", Deny: []string{"a", "b", "c"}},
		{Ref: "@vendor/dedup", Version: "1.0.0", Deny: []string{"a", "b"}},
	}
	for i, w := range want {
		if !reflect.DeepEqual(got.Uses[i], w) {
			t.Errorf("Uses[%d] = %+v, want %+v", i, got.Uses[i], w)
		}
	}
}

func TestParse_UsesWithoutDenyIsUnchanged(t *testing.T) {
	src := `FROM x
ENTRYPOINT y
USES @anthropic/web-search:1.2.0
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Uses[0].Deny != nil {
		t.Errorf("Deny = %v, want nil for USES without DENY clause", got.Uses[0].Deny)
	}
}

func TestParse_Ban(t *testing.T) {
	src := `FROM x
ENTRYPOINT y
USES @org/sub:1.0.0
BAN @org/weird-agent
BAN @org/web-search ONLY deep_crawl,external_fetch
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Ban{
		{Ref: "@org/weird-agent"},
		{Ref: "@org/web-search", Tools: []string{"deep_crawl", "external_fetch"}},
	}
	if !reflect.DeepEqual(got.Ban, want) {
		t.Errorf("Ban = %+v, want %+v", got.Ban, want)
	}
}

func TestParse_CaseInsensitive(t *testing.T) {
	src := `from python:3.12-slim
EnTrYpOiNt python3 -m agent
uses public @org/x:1.0
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.From != "python:3.12-slim" {
		t.Errorf("From = %q", got.From)
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
FROM python:3.12-slim

# blank line above, blank line below

ENTRYPOINT python3 -m agent
# trailing comment
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.From != "python:3.12-slim" || got.Entrypoint != "python3 -m agent" {
		t.Errorf("required fields not parsed: %+v", got)
	}
}

// Inline # is part of the value, matching Dockerfile; pip's url#sha256=
// pin specs depend on it.
func TestParse_InlineHashIsNotComment(t *testing.T) {
	src := `FROM python:3.12-slim # not a comment
RUN pip install foo @ https://example.com/foo.tar.gz#sha256=abc
ENTRYPOINT python3 -m agent
`
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.From != "python:3.12-slim # not a comment" {
		t.Errorf("From = %q, want the # to be preserved", got.From)
	}
	if len(got.Run) != 1 || got.Run[0] != "pip install foo @ https://example.com/foo.tar.gz#sha256=abc" {
		t.Errorf("Run = %v, want #sha256= preserved verbatim", got.Run)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"missing from",
			"ENTRYPOINT python3 -m agent",
			"FROM is required",
		},
		{
			"missing entrypoint",
			"FROM python:3.12-slim",
			"ENTRYPOINT is required",
		},
		{
			"unknown directive",
			"FROM x\nENTRYPOINT y\nFOOBAR baz",
			"unknown directive",
		},
		{
			"from declared twice",
			"FROM x\nFROM y\nENTRYPOINT z",
			"FROM declared twice",
		},
		{
			"entrypoint declared twice",
			"FROM x\nENTRYPOINT y\nENTRYPOINT z",
			"ENTRYPOINT declared twice",
		},
		{
			"model bad form",
			"FROM x\nENTRYPOINT y\nMODEL just-a-name",
			"MODEL must be provider/model-name",
		},
		{
			"uses latest tag rejected",
			"FROM x\nENTRYPOINT y\nUSES @anthropic/web-search:latest",
			"cannot use the latest tag",
		},
		{
			"uses missing version",
			"FROM x\nENTRYPOINT y\nUSES @anthropic/web-search",
			"must include a version tag",
		},
		{
			"uses missing @",
			"FROM x\nENTRYPOINT y\nUSES anthropic/web-search:1.0",
			"must start with @",
		},
		{
			"uses missing org slash name",
			"FROM x\nENTRYPOINT y\nUSES @web-search:1.0",
			"must be @org/name:version",
		},
		{
			"uses unknown keyword after ref",
			"FROM x\nENTRYPOINT y\nUSES @anthropic/web-search:1.0 ALLOW search",
			"expected DENY after reference",
		},
		{
			"uses DENY with no tools",
			"FROM x\nENTRYPOINT y\nUSES @anthropic/web-search:1.0 DENY",
			"DENY requires at least one tool name",
		},
		{
			"uses DENY with only separators",
			"FROM x\nENTRYPOINT y\nUSES @anthropic/web-search:1.0 DENY , ,",
			"DENY requires at least one non-empty tool name",
		},
		{
			"ban with a version",
			"FROM x\nENTRYPOINT y\nBAN @org/weird:1.0",
			"by name, not a version",
		},
		{
			"ban missing @",
			"FROM x\nENTRYPOINT y\nBAN org/weird",
			"must start with @",
		},
		{
			"ban missing org slash name",
			"FROM x\nENTRYPOINT y\nBAN @weird",
			"must be @org/name",
		},
		{
			"ban expects ONLY after ref",
			"FROM x\nENTRYPOINT y\nBAN @org/weird @org/other",
			"expected ONLY after reference",
		},
		{
			"ban ONLY with no tools",
			"FROM x\nENTRYPOINT y\nBAN @org/weird ONLY",
			"ONLY requires at least one tool name",
		},
		{
			"budget negative",
			"FROM x\nENTRYPOINT y\nBUDGET -1",
			"is not a USD amount",
		},
		{
			"budget extra args",
			"FROM x\nENTRYPOINT y\nBUDGET 1 USD",
			"takes a single USD amount",
		},
		{
			"budget bad form",
			"FROM x\nENTRYPOINT y\nBUDGET notanumber",
			"is not a USD amount",
		},
		{
			"budget too many decimals",
			"FROM x\nENTRYPOINT y\nBUDGET 5.0000001",
			"is not a USD amount",
		},
		{
			"resources unknown key",
			"FROM x\nENTRYPOINT y\nRESOURCES gpu=1",
			"unknown key",
		},
		{
			"resources bad pids",
			"FROM x\nENTRYPOINT y\nRESOURCES pids=0",
			"pids must be a positive integer",
		},
		{
			"resources negative cpu",
			"FROM x\nENTRYPOINT y\nRESOURCES cpu=-1",
			"cpu must be a positive number",
		},
		{
			"resources garbage mem",
			"FROM x\nENTRYPOINT y\nRESOURCES mem=lots",
			"mem must be a positive size",
		},
		{
			"reserved env prefix",
			"FROM x\nENTRYPOINT y\nENV AGENTCAGE_FOO=bar",
			"reserved AGENTCAGE_ prefix",
		},
		{
			"env empty key",
			"FROM x\nENTRYPOINT y\nENV =bar",
			"ENV requires a key",
		},
		{
			"egress bad format",
			"FROM x\nENTRYPOINT y\nEGRESS something",
			"must be deny-default or allow:",
		},
		{
			"eval declared twice",
			"FROM x\nENTRYPOINT y\nEVAL a\nEVAL b",
			"EVAL declared twice",
		},
		{
			"main empty",
			"FROM x\nENTRYPOINT y\nMAIN",
			"MAIN requires a tool name",
		},
		{
			"main declared twice",
			"FROM x\nENTRYPOINT y\nMAIN respond\nMAIN other",
			"MAIN declared twice",
		},
		{
			"main extra tokens",
			"FROM x\nENTRYPOINT y\nMAIN respond other",
			"MAIN takes a single tool name",
		},
		{
			"expose empty",
			"FROM x\nENTRYPOINT y\nEXPOSE",
			"EXPOSE requires at least one tool name",
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
