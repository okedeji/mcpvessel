package runtime

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/okedeji/agentcage/internal/progress"
)

// Setup phase names. These are the labels the operator sees in the
// `agentcage init` / first-run UI. They must match exactly what the
// Lima output tap dispatches via progress.Setup.Start, so keep them
// as constants instead of inline strings.
//
// We call the underlying Linux environment a "Linux VM" rather than
// "microVM" on purpose: in the agent-isolation world "microVM" almost
// always means per-agent Firecracker-style isolation, which is not
// what we do today. agentcage boots one shared Linux VM on macOS
// (via Lima + Apple Virtualization Framework) and runs agents as
// containers inside it. The phase name is honest about that.
const (
	SetupPhaseRuntime   = "Lima runtime ready"
	SetupPhasePreparing = "Preparing Linux VM"
	SetupPhaseBooting   = "Booting Linux VM"
)

// SetupPhases is the ordered phase list every setup UI shows. The
// orchestrator advances through them in order; phases the caller's
// environment skips (a VM that already exists has nothing to prepare,
// for example, or Lima found the Ubuntu image already cached and so
// "Preparing" had nothing to do) are auto-completed by the renderer
// when a later phase starts.
//
// There is intentionally no "Container runtime online" phase. By the
// time Lima reports READY, rootless containerd + buildkitd are already
// running inside the VM and the host sockets are forwarded. Adding a
// phase for it would just be a misleading 0.0s tick.
var SetupPhases = []string{
	SetupPhaseRuntime,
	SetupPhasePreparing,
	SetupPhaseBooting,
}

// SetupAlreadyReady reports whether the provisioner can skip
// EnsureBootstrap entirely. On macOS this means "Lima VM is already
// running"; on Linux native this is always true (no VM to provision).
//
// Returning true means the caller can proceed to BuildKit / nerdctl
// without showing any setup UI. False means EnsureBootstrap should be
// called and the UI is worth showing.
func SetupAlreadyReady(ctx context.Context, p Provisioner) bool {
	if l, ok := p.(*LimaProvisioner); ok {
		s, err := l.VM.Status(ctx)
		return err == nil && s == LimaRunning
	}
	// NativeProvisioner: no setup needed.
	return true
}

// EnsureBootstrap runs the provisioner's setup flow with phase-aware
// progress UI piped to ui, suppressing Lima's raw output. Operators
// who want the firehose pass verbose=true; then ui is ignored and
// Lima's stdout/stderr stream directly to verboseOut.
//
// When ui is nil (called from a non-interactive context that has no
// UI but still wants quiet operation), the function runs silently and
// returns when EnsureReady completes.
//
// On Linux native this is a no-op (no VM to provision) and returns
// nil immediately without writing to ui.
func EnsureBootstrap(ctx context.Context, p Provisioner, ui progress.Setup, verbose bool, verboseOut io.Writer) error {
	if _, ok := p.(*NativeProvisioner); ok {
		if ui != nil {
			// Walk the UI through every phase so the operator sees
			// the same shape on Linux as on macOS; Start auto-
			// completes pending phases ahead of the named one.
			ui.Start(SetupPhaseBooting)
			ui.Done()
		}
		return p.EnsureReady(ctx, io.Discard, io.Discard)
	}

	if verbose {
		// Skip the UI entirely; the operator wants Lima's raw output.
		return p.EnsureReady(ctx, verboseOut, verboseOut)
	}

	if ui != nil {
		ui.Start(SetupPhaseRuntime)
	}

	// Tee Lima's stderr stream into our phase parser. stdout is
	// rarely used by limactl for the create/start path but we tap it
	// too just in case so progress detail does not depend on which
	// stream a given Lima version chose.
	tap := newSetupTap(ui)
	if err := p.EnsureReady(ctx, tap, tap); err != nil {
		if ui != nil {
			ui.Fail(err)
		}
		return err
	}

	if ui != nil {
		// Lima's "READY." means containerd + buildkitd are already
		// running inside the VM and the host sockets are forwarded.
		// Mark every still-pending phase done via the last one.
		ui.Start(SetupPhaseBooting)
		ui.Done()
	}
	return nil
}

// setupTap is an io.Writer that splits incoming bytes into lines,
// scans each line for known Lima markers, and drives a progress.Setup
// accordingly. Unmatched lines are discarded; the UI is the only
// thing the operator sees in non-verbose mode.
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

// handleLine is the phase-detection logic. We watch for line
// substrings rather than full regex matches so Lima output shape
// changes between point releases do not silently break us. At worst
// a marker stops firing and the operator sees the spinner stay on the
// previous phase for a few extra seconds.
//
// Markers below are derived from Lima 2.x output observed on macOS;
// see the smoke-test transcripts for examples.
//
// Order matters in the phase switch: "READY." short-circuits because
// it must not restart Booting. "[hostagent]" comes next; once the
// host agent reports anything, the VM is alive and we are in Booting.
// "Created an instance" similarly: it is emitted at the end of
// `limactl create`, after the downloads, so it cleanly marks the
// create → start boundary even when no `[hostagent]` line appears
// quickly. Only then do we look at the Preparing markers, so a
// download line after creation does not bounce us back.
func (t *setupTap) handleLine(line string) {
	if t.ui == nil {
		return
	}
	low := strings.ToLower(line)

	switch {
	case strings.Contains(line, "READY."):
		// EnsureBootstrap completes the booting phase itself after
		// EnsureReady returns; nothing to do here.

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

	// Optional detail line. Lima emits human-friendly status the
	// hostagent already brokered; we surface the slow ones so the
	// operator knows the spinner is making progress.
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

// trimLimaPrefix strips the `time="..." level=info msg="..."` framing
// that Lima sometimes prepends so the detail line in our UI shows
// only the human-meaningful tail.
func trimLimaPrefix(line string) string {
	// INFO[0030] [hostagent] Downloading ...
	if i := strings.Index(line, "] "); i >= 0 && (strings.HasPrefix(line, "INFO[") || strings.HasPrefix(line, "WARN[")) {
		rest := strings.TrimPrefix(line[i+2:], "[hostagent] ")
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(line)
}

// SetupTitle and SetupTip are the canned text the setup UI shows above
// and below the phase list. Centralized so `agentcage init` and the
// auto-bootstrap path say the same thing.
const (
	SetupTitle = "First-time setup (one-time, takes 2-5 minutes)"
	SetupTip   = "Tip: agentcage caches everything in ~/.agentcage; later runs take seconds."
)

// NewSetupUI is the canonical constructor for the setup UI. Picks TTY
// or Plain mode based on the writer; chooses the right title/tip pair.
func NewSetupUI(w io.Writer) progress.Setup {
	return progress.NewSetup(w, SetupTitle, SetupTip, SetupPhases)
}

// FirstRunDetected reports whether the host has a Lima VM provisioned
// for agentcage already. False on first run (so the CLI can announce
// "first-time setup" up front rather than mid-stream).
//
// Returns true on Linux native (no VM concept) so the CLI does not
// show the setup banner there.
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

// VerboseOutput resolves a writer for verbose mode. Returns the
// passed writer if non-nil, falls back to os.Stderr so a caller who
// forgot to wire one still sees output instead of nothing.
func VerboseOutput(w io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return os.Stderr
}
