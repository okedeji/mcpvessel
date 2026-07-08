// Package replay owns the .replay artifact under ~/.agentcage/replays: the
// full-payload recording of a run's external interactions. The daemon
// assembles a Recording at a run's finish; readers decode the same shape.
package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/okedeji/agentcage/internal/env"
)

// Version is the artifact schema version, bumped only on breaking changes.
const Version = "0.1"

const replaysDirName = "replays"

// Recording is one run's input, ordered events, and result. StartedAt and
// EndedAt bound the run; events carry their own times.
type Recording struct {
	Version      string    `json:"version"`
	AgentRef     string    `json:"agent_ref"`
	AgentVersion string    `json:"agent_version,omitempty"`
	ManifestHash string    `json:"agent_manifest_hash,omitempty"`
	RunID        string    `json:"run_id"`
	Input        Input     `json:"input"`
	Events       []Event   `json:"events"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	Result       Result    `json:"result"`
}

// Input is the tools/call that started the run.
type Input struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// Result is the run's final return value or error, with the terminal status.
type Result struct {
	Output string `json:"output,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Event is one recorded external interaction, ordered by Seq. An LLM call
// carries its full request, response, and metered cost; a sub-agent call
// (typed "subagent.<edge>.<tool>") carries its args and response. Request and
// Response are raw JSON when the payload was JSON, a JSON string otherwise
// (a streamed SSE body), keeping the artifact valid JSON either way.
type Event struct {
	Seq          int             `json:"seq"`
	Type         string          `json:"type"`
	Request      json.RawMessage `json:"request,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	TokensIn     int64           `json:"tokens_in,omitempty"`
	TokensOut    int64           `json:"tokens_out,omitempty"`
	CostMicroUSD int64           `json:"cost_micro_usd,omitempty"`
	TUnixNano    int64           `json:"t_unix_nano,omitempty"`
}

// Event types.
const (
	EventLLMComplete = "llm.complete"
	EventLLMStream   = "llm.stream"
)

// Path is ~/.agentcage/replays/<run-id>.replay, honoring AGENTCAGE_HOME.
func Path(runID string) (string, error) {
	home, err := env.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, replaysDirName, runID+".replay"), nil
}

// Write encodes the recording to the run's .replay file, creating the replays
// directory if needed.
func Write(rec *Recording) error {
	path, err := Path(rec.RunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding recording: %w", err)
	}
	return os.WriteFile(path, buf, 0o600)
}

// RawOrString embeds a captured payload: the bytes themselves when valid
// JSON, a JSON-encoded string otherwise (a streamed SSE body).
func RawOrString(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	s, err := json.Marshal(string(b))
	if err != nil {
		return nil
	}
	return s
}
