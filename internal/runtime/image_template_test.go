package runtime

import (
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/agentfile"
)

func TestGenerateDockerfile_Minimal(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: "python3 -m agent",
	}
	got := generateDockerfile(dockerfileInput{Agentfile: af})

	wantLines := []string{
		"FROM python:3.12-slim",
		"WORKDIR /agent",
		"COPY . /agent",
		`ENTRYPOINT ["sh", "-c", "python3 -m agent"]`,
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
}

func TestGenerateDockerfile_RunStepsInOrder(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "node:20-slim",
		Entrypoint: "node dist/server.js",
		Run: []string{
			"npm ci",
			"npm run build",
		},
	}
	got := generateDockerfile(dockerfileInput{Agentfile: af})

	npmCI := strings.Index(got, "RUN npm ci")
	npmBuild := strings.Index(got, "RUN npm run build")
	if npmCI < 0 || npmBuild < 0 {
		t.Fatalf("RUN steps missing:\n%s", got)
	}
	if npmCI > npmBuild {
		t.Errorf("RUN steps emitted out of order")
	}
}

func TestGenerateDockerfile_EnvDeterministic(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: "python3 main.py",
		Env: map[string]string{
			"LOG_LEVEL": "info",
			"TZ":        "UTC",
			"TIMEOUT":   "30",
		},
	}
	// Run codegen twice; output must be byte-identical regardless of
	// Go's map iteration order, otherwise BuildKit's cache key thrashes
	// on every build.
	first := generateDockerfile(dockerfileInput{Agentfile: af})
	for i := 0; i < 10; i++ {
		if got := generateDockerfile(dockerfileInput{Agentfile: af}); got != first {
			t.Fatalf("non-deterministic codegen: ENV map iteration leaked into output")
		}
	}
}

func TestGenerateDockerfile_EnvValueWithSpaces(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: "python3 main.py",
		Env: map[string]string{
			"GREETING": "hello world",
		},
	}
	got := generateDockerfile(dockerfileInput{Agentfile: af})
	if !strings.Contains(got, `ENV GREETING="hello world"`) {
		t.Errorf("ENV value with spaces not quoted:\n%s", got)
	}
}

func TestGenerateDockerfile_EnvValueSimple(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: "python3 main.py",
		Env: map[string]string{
			"LEVEL": "info",
		},
	}
	got := generateDockerfile(dockerfileInput{Agentfile: af})
	if !strings.Contains(got, "ENV LEVEL=info\n") {
		t.Errorf("simple ENV value should not be quoted:\n%s", got)
	}
}

func TestGenerateDockerfile_LabelsSortedAndQuoted(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: "python3 main.py",
	}
	got := generateDockerfile(dockerfileInput{
		Agentfile: af,
		Labels: map[string]string{
			"io.agentcage.spec_version": "1",
			"io.agentcage.agent_ref":    "@okedeji/researcher:1.0",
			"io.agentcage.built_at":     "2026-06-07T00:00:00Z",
		},
	})

	idxAgentRef := strings.Index(got, "LABEL io.agentcage.agent_ref")
	idxBuiltAt := strings.Index(got, "LABEL io.agentcage.built_at")
	idxSpecVersion := strings.Index(got, "LABEL io.agentcage.spec_version")
	if idxAgentRef < 0 || idxBuiltAt < 0 || idxSpecVersion < 0 {
		t.Fatalf("expected labels missing:\n%s", got)
	}
	// Alphabetical: agent_ref < built_at < spec_version
	if idxAgentRef >= idxBuiltAt || idxBuiltAt >= idxSpecVersion {
		t.Errorf("labels not emitted in sorted order")
	}
	if !strings.Contains(got, `LABEL io.agentcage.agent_ref="@okedeji/researcher:1.0"`) {
		t.Errorf("label value not quoted:\n%s", got)
	}
}

func TestGenerateDockerfile_EmptyLabelsSkipped(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: "python3 main.py",
	}
	got := generateDockerfile(dockerfileInput{
		Agentfile: af,
		Labels: map[string]string{
			"io.agentcage.empty":   "",
			"io.agentcage.present": "yes",
		},
	})
	if strings.Contains(got, "io.agentcage.empty") {
		t.Errorf("empty-valued label should be skipped:\n%s", got)
	}
	if !strings.Contains(got, "io.agentcage.present") {
		t.Errorf("non-empty label should be present:\n%s", got)
	}
}

func TestGenerateDockerfile_EntrypointQuoting(t *testing.T) {
	// Multi-token entrypoint with spaces must be quoted as a single
	// shell -c argument so Dockerfile parses it as JSON-form ENTRYPOINT
	// with three elements: sh, -c, the full command string.
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: `python3 -m agent --flag "value"`,
	}
	got := generateDockerfile(dockerfileInput{Agentfile: af})
	if !strings.Contains(got, `ENTRYPOINT ["sh", "-c", "python3 -m agent --flag \"value\""]`) {
		t.Errorf("entrypoint with embedded quotes not escaped:\n%s", got)
	}
}

func TestGenerateDockerfile_SyntaxDirectivePresent(t *testing.T) {
	af := &agentfile.Agentfile{
		From:       "python:3.12-slim",
		Entrypoint: "python3 main.py",
	}
	got := generateDockerfile(dockerfileInput{Agentfile: af})
	// The "# syntax=" parser directive must appear early so BuildKit
	// pulls the named frontend before processing the rest of the file.
	if !strings.HasPrefix(got, "# Auto-generated") {
		t.Errorf("expected auto-generated header")
	}
	if !strings.Contains(got, "# syntax=docker/dockerfile:1") {
		t.Errorf("missing # syntax= parser directive:\n%s", got)
	}
}
