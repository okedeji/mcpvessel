package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SweepDaemonOrphans removes the containers and networks a previous daemon left
// behind. A daemon-managed run dies with the daemon that held its stdio, but its
// detached sub-agents, gateways, and networks do not, so the daemon labels them
// and a freshly started one sweeps any that survive a crash. One-shot runs carry
// no such label and are never touched.
//
// The caller must invoke this only once it owns the control socket, so a daemon
// already serving cannot have its live runs swept. It is best-effort: with no
// runtime up there is nothing to sweep, and a nerdctl hiccup returns for the
// caller to log rather than aborting startup. Containers go before networks so a
// network is empty by the time it is removed.
func SweepDaemonOrphans(ctx context.Context) error {
	p, err := DefaultProvisioner()
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	if !SetupAlreadyReady(ctx, p) {
		return nil
	}

	var errs []error
	containers, err := nerdctlLines(ctx, p, "ps", "-aq", "--filter", "label="+daemonResourceLabel)
	if err != nil {
		errs = append(errs, err)
	}
	for _, id := range containers {
		if err := removeContainer(p, id); err != nil {
			errs = append(errs, err)
		}
	}

	networks, err := nerdctlLines(ctx, p, "network", "ls", "-q", "--filter", "label="+daemonResourceLabel)
	if err != nil {
		errs = append(errs, err)
	}
	for _, id := range networks {
		if err := removeNetwork(p, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// nerdctlLines runs nerdctl and returns its stdout split into non-empty trimmed
// lines, the shape the -q listings the sweep reads come back in.
func nerdctlLines(ctx context.Context, p Provisioner, args ...string) ([]string, error) {
	cmd := p.Nerdctl(ctx, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("nerdctl %s: %w", strings.Join(args, " "), err)
	}
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}
