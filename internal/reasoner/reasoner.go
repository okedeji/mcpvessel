// Package reasoner carries the Python reasoning harness that
// `import --reasoning` writes into a generated agent, and renders that
// agent's Agentfile. The harness ships as source so the operator can read
// and edit it in their own directory.
package reasoner

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed reasoner.py
var harness []byte

// HarnessFileName is the harness's filename in a generated agent.
const HarnessFileName = "reasoner.py"

// HarnessSource is the reasoning-loop source written into a generated agent.
func HarnessSource() []byte { return harness }

const base = "python:3.12-slim"

// deferredModel is the MODEL carried when --model is not given. Its provider
// is deliberately not one an operator configures; the LLM gateway resolves it
// to the operator's default provider/model, so nothing ages.
const deferredModel = "default/default"

// Params configure a generated reasoning agent.
type Params struct {
	// UsesRefs are the tool-collection references the agent reasons over, one
	// USES edge each. The harness aggregates every edge's tools into one menu.
	UsesRefs []string
	// Model is a provider/model to pin, or empty to defer to the operator's default.
	Model string
	// SystemPrompt is the reasoning prompt, or empty to use the harness default.
	SystemPrompt string
}

// Agentfile renders the reasoning agent that runs the harness over its USES
// tools.
func Agentfile(p Params) (string, error) {
	if len(p.UsesRefs) == 0 {
		return "", fmt.Errorf("reasoner: no tool-collection ref to USES")
	}
	model := p.Model
	if model == "" {
		model = deferredModel
	}

	lines := []string{
		"FROM " + base,
		"RUN pip install --no-cache-dir mcp openai",
		"MODEL " + model,
		"MAIN respond",
	}
	for _, ref := range p.UsesRefs {
		lines = append(lines, "USES "+ref)
	}
	if p.SystemPrompt != "" {
		// Not an AGENTCAGE_ name: the parser reserves that prefix for the
		// runtime's own injected variables.
		lines = append(lines, "ENV REASONER_SYSTEM_PROMPT="+strings.ReplaceAll(p.SystemPrompt, "\n", " "))
	}
	lines = append(lines, "ENTRYPOINT python3 "+HarnessFileName)
	return strings.Join(lines, "\n") + "\n", nil
}
