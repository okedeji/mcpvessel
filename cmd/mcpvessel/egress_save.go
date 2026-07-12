package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/progress"
)

// saveEgress persists an operator's --egress into editable targets and rebuilds
// them, so the hosts are baked into the bundle rather than allowed for one run.
// A target that is not a source directory cannot be edited (a pulled, signed
// bundle has no local source), so it is a clear error rather than a silent
// no-op. args are the raw serve/run arguments; scoped is the parsed --egress.
func saveEgress(ctx context.Context, stderr io.Writer, args []string, scoped map[string][]string, env, secrets map[string]string) error {
	for _, arg := range args {
		hosts := egress.HostsFor(scoped, filepath.Base(arg))
		if len(hosts) == 0 {
			continue
		}
		info, err := os.Stat(arg)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("--save needs a source directory to write EGRESS into, but %q is a built bundle. Allow it for this run with --egress alone, or re-import the server with --egress and rebuild", arg)
		}
		vf := filepath.Join(arg, bundle.VesselfileName)
		if _, err := os.Stat(vf); err != nil {
			return fmt.Errorf("--save: %s has no %s to edit", arg, bundle.VesselfileName)
		}
		if err := setVesselfileEgress(vf, hosts); err != nil {
			return err
		}
		// The rebuild reintrospects the server, so it needs the same inputs the
		// server needs to boot.
		if _, _, err := buildIntoStore(ctx, stderr, stderr, buildConfig{srcDir: arg, mode: progress.ModeAuto, env: env, secrets: secrets}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stderr, "Saved EGRESS allow:%s to %s and rebuilt.\n", strings.Join(hosts, ","), vf)
	}
	return nil
}

// setVesselfileEgress unions hosts into the Vesselfile's EGRESS allow: line,
// adding the directive when absent. Existing hosts are kept; order is stable so
// a re-save is a no-op diff.
func setVesselfileEgress(path string, add []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")

	seen := map[string]bool{}
	var hosts []string
	addHost := func(h string) {
		if h = strings.TrimSpace(h); h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}

	idx := -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if idx == -1 && strings.HasPrefix(strings.ToUpper(t), "EGRESS ") {
			idx = i
			rest := strings.TrimSpace(t[len("EGRESS "):])
			if strings.HasPrefix(rest, "allow:") {
				for _, h := range strings.Split(strings.TrimPrefix(rest, "allow:"), ",") {
					addHost(h)
				}
			}
		}
	}
	for _, h := range add {
		addHost(h)
	}

	newLine := "EGRESS allow:" + strings.Join(hosts, ",")
	if idx >= 0 {
		lines[idx] = newLine
	} else {
		lines = append(lines, newLine)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}
