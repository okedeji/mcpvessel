package runtime

import (
	"fmt"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
)

func TestNeedsGatewayImage(t *testing.T) {
	plain := mustTree(t, agent("main", nil), nil)
	if needsGatewayImage(plain) {
		t.Error("plain single cage needs no gateway image")
	}

	reasoning := &bundle.Manifest{Agentfile: bundle.AgentfileSpec{Main: "respond", Model: "openai/gpt-4o"}}
	if !needsGatewayImage(mustTree(t, reasoning, nil)) {
		t.Error("a reasoning node starts the LLM gateway; its image is needed")
	}

	const dig = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	withUses := mustTree(t,
		agent("main", nil, use("@org/sub", "0.1", dig)),
		map[string]*bundle.Manifest{dig: agent("", []string{"fetch"})})
	if !needsGatewayImage(withUses) {
		t.Error("a tree with edges starts the MCP gateway; its image is needed")
	}
}

func TestIsTransientBuildError(t *testing.T) {
	transient := []string{
		"failed to solve: python:3.12-slim: failed to resolve source metadata: unexpected status from HEAD request: 500 Internal Server Error",
		"read tcp: connection reset by peer",
		"net/http: TLS handshake timeout",
		"toomanyrequests: rate limited",
	}
	for _, msg := range transient {
		if !isTransientBuildError(fmt.Errorf("%s", msg)) {
			t.Errorf("not retried, want retry: %s", msg)
		}
	}
	permanent := []string{
		"failed to resolve source metadata for docker.io/library/pythn:3.12-slim: not found",
		"process \"/bin/sh -c pip install nope\" did not complete successfully: exit code: 1",
	}
	for _, msg := range permanent {
		if isTransientBuildError(fmt.Errorf("%s", msg)) {
			t.Errorf("retried, want no retry: %s", msg)
		}
	}
}
