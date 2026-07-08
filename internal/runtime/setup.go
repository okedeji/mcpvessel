package runtime

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/okedeji/agentcage/internal/progress"
)

// Setup phase names, shown in the init / first-run UI. They must match what
// the Lima output tap dispatches via progress.Setup.Start.
//
// "Linux VM", not "microVM": agentcage boots one shared Lima VM and runs
// agents as containers inside it, not per-agent Firecracker-style isolation.
const (
	SetupPhaseRuntime   = "Lima runtime ready"
	SetupPhasePreparing = "Preparing Linux VM"
	SetupPhaseBooting   = "Booting Linux VM"
)

// SetupPhases is the ordered phase list every setup UI shows. Phases the
// environment skips are auto-completed by the renderer when a later phase
// starts. There is no "Container runtime online" phase: by the time Lima
// reports READY, containerd + buildkitd are already running and the host
// sockets forwarded, so it would be a misleading 0.0s tick.
var SetupPhases = []string{
	SetupPhaseRuntime,
	SetupPhasePreparing,
	SetupPhaseBooting,
}

// SetupAlreadyReady reports whether the provisioner can skip EnsureBootstrap:
// the Lima VM is already running on macOS, always on Linux native.
func SetupAlreadyReady(ctx context.Context, p Provisioner) bool {
	if l, ok := p.(*LimaProvisioner); ok {
		s, err := l.VM.Status(ctx)
		return err == nil && s == LimaRunning
	}
	return true
}

// EnsureBootstrap runs the provisioner's setup flow behind the phase UI,
// suppressing Lima's raw output. verbose=true bypasses ui and streams Lima's
// stdout/stderr to verboseOut. A nil ui runs silently.
func EnsureBootstrap(ctx context.Context, p Provisioner, ui progress.Setup, verbose bool, verboseOut io.Writer) error {
	if _, ok := p.(*NativeProvisioner); ok {
		if ui != nil {
			// Walk the UI through every phase so Linux shows the same
			// shape as macOS; Start auto-completes the pending ones.
			ui.Start(SetupPhaseBooting)
			ui.Done()
		}
		return p.EnsureReady(ctx, io.Discard, io.Discard)
	}

	if verbose {
		return p.EnsureReady(ctx, verboseOut, verboseOut)
	}

	if ui != nil {
		ui.Start(SetupPhaseRuntime)
	}

	// Tap both streams; which one limactl uses varies by version.
	tap := newSetupTap(ui)
	if err := p.EnsureReady(ctx, tap, tap); err != nil {
		if ui != nil {
			ui.Fail(err)
		}
		return err
	}

	if ui != nil {
		// Lima's "READY." means containerd + buildkitd are up and the
		// sockets forwarded; complete every pending phase via the last one.
		ui.Start(SetupPhaseBooting)
		ui.Done()
	}
	return nil
}

// setupTap splits incoming bytes into lines, scans for known Lima markers,
// and drives a progress.Setup. Unmatched lines are discarded.
type setupTap struct {
	ui  progress.Setup
	buf []byte
}

func newSetupTap(ui progress.Setup) *setupTap {
	return &setupTap{ui: ui, buf: make([]byte, 0, 4096)}
}

func (t *setupTap) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		idx := indexByte(t.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(t.buf[:idx])
		t.buf = t.buf[idx+1:]
		t.handleLine(line)
	}
	return len(p), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// handleLine watches for substrings rather than regexes so Lima output shape
// changes between point releases degrade gracefully: at worst a marker stops
// firing and the spinner lingers on the previous phase. Markers come from
// Lima 2.x output on macOS.
//
// Order matters: "READY." short-circuits so it never restarts Booting;
// "[hostagent]" and "created an instance" mark the create/start boundary
// ahead of the Preparing markers, so a download line after creation does not
// bounce the phase back.
func (t *setupTap) handleLine(line string) {
	if t.ui == nil {
		return
	}
	low := strings.ToLower(line)

	switch {
	case strings.Contains(line, "READY."):
		// EnsureBootstrap completes the booting phase itself.

	case strings.Contains(low, "[hostagent]"),
		strings.Contains(low, "starting the instance"),
		strings.Contains(low, "starting instance"),
		strings.Contains(low, "boot scripts"),
		strings.Contains(low, "waiting for the guest agent"),
		strings.Contains(low, "created an instance"):
		t.ui.Start(SetupPhaseBooting)

	case strings.Contains(low, "creating an instance"),
		strings.Contains(low, "pulling:"),
		strings.Contains(low, "downloading"):
		t.ui.Start(SetupPhasePreparing)
	}

	// Detail lines: surface the slow steps so the operator knows the spinner
	// is making progress.
	switch {
	case strings.Contains(low, "downloading"):
		t.ui.SetDetail(trimLimaPrefix(line))
	case strings.Contains(low, "pulling:"):
		t.ui.SetDetail(trimLimaPrefix(line))
	case strings.Contains(low, "waiting for the final requirement"):
		t.ui.SetDetail("waiting for boot scripts")
	case strings.Contains(low, "waiting for the guest agent"):
		t.ui.SetDetail("waiting for guest agent")
	}
}

// trimLimaPrefix strips Lima's log framing so the detail line shows only the
// human-meaningful tail.
func trimLimaPrefix(line string) string {
	// INFO[0030] [hostagent] Downloading ...
	if i := strings.Index(line, "] "); i >= 0 && (strings.HasPrefix(line, "INFO[") || strings.HasPrefix(line, "WARN[")) {
		rest := strings.TrimPrefix(line[i+2:], "[hostagent] ")
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(line)
}

// SetupTitle and SetupTip are the canned setup UI text, centralized so init
// and the auto-bootstrap path say the same thing.
const (
	SetupTitle = "First-time setup (one-time, takes 2-5 minutes)"
	SetupTip   = "Tip: agentcage caches everything in ~/.agentcage; later runs take seconds."
)

// NewSetupUI constructs the setup UI, picking TTY or plain mode from the
// writer.
func NewSetupUI(w io.Writer) progress.Setup {
	return progress.NewSetup(w, SetupTitle, SetupTip, SetupPhases)
}

// FirstRunDetected reports whether this host still needs its first-time Lima
// setup: true only when no agentcage VM exists yet. Always false on Linux
// native, which has no VM.
func FirstRunDetected(ctx context.Context, p Provisioner) bool {
	if _, ok := p.(*NativeProvisioner); ok {
		return false
	}
	l, ok := p.(*LimaProvisioner)
	if !ok {
		return false
	}
	s, err := l.VM.Status(ctx)
	if err != nil {
		return false
	}
	return s == LimaNonexistent
}

// VerboseOutput returns w, or os.Stderr when nil.
func VerboseOutput(w io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return os.Stderr
}
