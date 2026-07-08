package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
)

// CageStat is one cage's live resource usage. The values are nerdctl's
// already-formatted strings, kept as-is so the operator sees the same numbers
// nerdctl reports.
type CageStat struct {
	Name string `json:"name"`
	CPU  string `json:"cpu"`
	Mem  string `json:"mem"`
	PIDs string `json:"pids"`
}

// HostStats snapshots every running cage's resource usage. ok is false when
// the runtime is not up (no VM, no daemon), a benign condition rather than an
// error.
func HostStats(ctx context.Context) ([]CageStat, bool) {
	p, err := DefaultProvisioner()
	if err != nil {
		return nil, false
	}
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(ctx, containerStopTimeout)
	defer cancel()
	cmd := p.Nerdctl(ctx, "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return nil, false
	}
	return parseStats(out.String()), true
}

// parseStats reads nerdctl's per-container JSON stats lines.
func parseStats(out string) []CageStat {
	var stats []CageStat
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			Name     string `json:"Name"`
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
			PIDs     string `json:"PIDs"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		stats = append(stats, CageStat{Name: row.Name, CPU: row.CPUPerc, Mem: row.MemUsage, PIDs: row.PIDs})
	}
	return stats
}
