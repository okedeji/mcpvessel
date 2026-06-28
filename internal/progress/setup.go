package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Setup renders a multi-phase first-time-setup flow. Phases are defined
// up front; the orchestrator calls Start to advance from one to the
// next, optionally SetDetail to attach a sub-status to the active
// phase ("downloading 342 MB" inside "Preparing Linux microVM"), and
// Done when all phases are complete.
//
// Two implementations ship: SetupTTY (live-updating, spinner + elapsed
// times), and SetupPlain (one line per phase transition, safe for pipes
// / CI capture buffers). NewSetup picks based on whether the writer
// is a terminal.
//
// The renderer suppresses Lima's raw output entirely. Users see a
// branded, calm first-time experience instead of a hundred INFO lines
// they cannot interpret. Operators who want the raw stream pass
// --verbose to bypass the renderer.
type Setup interface {
	// Start marks the named phase active and completes the previously
	// active one. Phase names must match what was passed to NewSetup
	// at construction. Calling Start for a phase not in the list is a
	// no-op (the renderer never fails the caller for a label typo).
	Start(name string)
	// SetDetail attaches a short sub-status to the active phase. The
	// detail is rendered next to the phase name and overwritten by
	// later SetDetail calls. Empty strings clear the detail.
	SetDetail(detail string)
	// Done completes the active phase and stops the renderer's tick
	// loop. Safe to call more than once.
	Done()
	// Fail marks the active phase as failed and stops the renderer.
	// The error is shown next to the phase in the final render.
	Fail(err error)
}

// NewSetup picks SetupTTY when w is a terminal, SetupPlain otherwise.
// title and tip are displayed above and below the phase list (the tip
// can be empty). phases are rendered in order; the first call to
// Start activates the first one.
func NewSetup(w io.Writer, title, tip string, phases []string) Setup {
	if IsTerminal(w) {
		return NewSetupTTY(w, title, tip, phases)
	}
	return NewSetupPlain(w, title, tip, phases)
}

type phaseState int

const (
	phasePending phaseState = iota
	phaseActive
	phaseDone
	phaseFailed
)

type setupPhase struct {
	name      string
	state     phaseState
	detail    string
	err       error
	startedAt time.Time
	endedAt   time.Time
}

// findPhase returns the index of the phase with the given name, or -1
// if there is none. Match is case-sensitive; phase names are
// developer-supplied strings, not user input.
func findPhase(phases []setupPhase, name string) int {
	for i, p := range phases {
		if p.name == name {
			return i
		}
	}
	return -1
}

func activePhaseIndex(phases []setupPhase) int {
	for i, p := range phases {
		if p.state == phaseActive {
			return i
		}
	}
	return -1
}

// SetupTTY is the live-updating renderer. It owns a tick goroutine
// that refreshes the display so elapsed times update smoothly while a
// phase runs. Done stops the goroutine.
type SetupTTY struct {
	w       io.Writer
	mu      sync.Mutex
	title   string
	tip     string
	phases  []setupPhase
	started time.Time
	stop    chan struct{}
	once    sync.Once
	lines   int // lines emitted in the previous render
}

// NewSetupTTY constructs the live renderer. It writes the initial
// header immediately so the user sees something on the first frame
// even before the first Start.
func NewSetupTTY(w io.Writer, title, tip string, phaseNames []string) *SetupTTY {
	phases := make([]setupPhase, len(phaseNames))
	for i, n := range phaseNames {
		phases[i].name = n
	}
	t := &SetupTTY{
		w:       w,
		title:   title,
		tip:     tip,
		phases:  phases,
		started: time.Now(),
		stop:    make(chan struct{}),
	}
	go t.tickLoop()
	t.render()
	return t
}

func (t *SetupTTY) tickLoop() {
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.render()
		}
	}
}

func (t *SetupTTY) Start(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	idx := findPhase(t.phases, name)
	if idx < 0 {
		return
	}
	// Re-calling Start for the already-active phase is a no-op. The
	// tap fires Start whenever it sees a marker, and most phases
	// have many matching markers, and without this guard each marker
	// would reset startedAt and clear detail, making the UI feel
	// stuck at "0s" while real time elapses.
	if t.phases[idx].state == phaseActive {
		return
	}
	now := time.Now()
	// Complete the currently-active phase, then auto-complete any
	// still-pending phases that sit before the new one. This keeps
	// the UI honest when our Lima tap misses a marker (or when a
	// phase legitimately has no work, e.g. the Ubuntu image was
	// cached so "Preparing" had nothing to do). Without this,
	// skipped phases stay with a leading dot forever.
	if cur := activePhaseIndex(t.phases); cur >= 0 {
		t.phases[cur].state = phaseDone
		t.phases[cur].endedAt = now
	}
	for i := range idx {
		if t.phases[i].state == phasePending {
			t.phases[i].state = phaseDone
			t.phases[i].startedAt = now
			t.phases[i].endedAt = now
		}
	}
	t.phases[idx].state = phaseActive
	t.phases[idx].startedAt = now
	t.phases[idx].detail = ""
	t.renderLocked()
}

func (t *SetupTTY) SetDetail(detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur := activePhaseIndex(t.phases); cur >= 0 {
		t.phases[cur].detail = detail
	}
	t.renderLocked()
}

func (t *SetupTTY) Done() {
	t.once.Do(func() {
		t.mu.Lock()
		if cur := activePhaseIndex(t.phases); cur >= 0 {
			t.phases[cur].state = phaseDone
			t.phases[cur].endedAt = time.Now()
		}
		t.mu.Unlock()
		close(t.stop)
		t.render()
	})
}

func (t *SetupTTY) Fail(err error) {
	t.once.Do(func() {
		t.mu.Lock()
		if cur := activePhaseIndex(t.phases); cur >= 0 {
			t.phases[cur].state = phaseFailed
			t.phases[cur].endedAt = time.Now()
			t.phases[cur].err = err
		}
		t.mu.Unlock()
		close(t.stop)
		t.render()
	})
}

func (t *SetupTTY) render() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.renderLocked()
}

// spinnerFrames is the Braille-dot spinner shared with most other
// modern CLI tools (rustup, cargo, gh). Indexed by elapsed
// tickInterval slices so adjacent renders show distinct frames.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerFrame(elapsed time.Duration) string {
	return spinnerFrames[int(elapsed/tickInterval)%len(spinnerFrames)]
}

func (t *SetupTTY) renderLocked() {
	if t.lines > 0 {
		_, _ = fmt.Fprintf(t.w, "\033[%dA", t.lines)
	}

	var b strings.Builder
	if t.title != "" {
		writeLine(&b, t.title)
		writeLine(&b, "")
	}
	lines := 0
	if t.title != "" {
		lines += 2
	}
	for _, p := range t.phases {
		writeLine(&b, formatSetupPhase(p))
		lines++
	}
	if t.tip != "" {
		writeLine(&b, "")
		writeLine(&b, t.tip)
		lines += 2
	}

	_, _ = io.WriteString(t.w, b.String())
	t.lines = lines
}

// writeLine appends s followed by the clear-to-end-of-line escape so
// leftover characters from a previous, longer render do not bleed
// through.
func writeLine(b *strings.Builder, s string) {
	b.WriteString(s)
	b.WriteString("\033[K\n")
}

// formatSetupPhase renders one phase line. Format:
//
//	✓ <name>                                  3s
//	⠼ <name> - <detail>                  1m 22s
//	· <name>
//	✗ <name> - <error>                  (failed)
//
// The duration is left out for pending phases (they have no clock).
func formatSetupPhase(p setupPhase) string {
	var icon string
	switch p.state {
	case phaseDone:
		icon = "✓"
	case phaseActive:
		var dur time.Duration
		if !p.startedAt.IsZero() {
			dur = time.Since(p.startedAt)
		}
		icon = spinnerFrame(dur)
	case phaseFailed:
		icon = "✗"
	default:
		icon = "·"
	}

	label := p.name
	if p.detail != "" && (p.state == phaseActive || p.state == phaseDone) {
		label = label + " - " + p.detail
	}
	if p.state == phaseFailed && p.err != nil {
		label = label + " - " + p.err.Error()
	}

	var right string
	switch p.state {
	case phaseDone:
		right = humanDuration(p.endedAt.Sub(p.startedAt))
	case phaseActive:
		right = humanDuration(time.Since(p.startedAt))
	case phaseFailed:
		right = "(failed)"
	}

	return formatTwoColumn("  "+icon+" "+label, right)
}

// formatTwoColumn pads left with spaces so right ends at column 78. If
// the line would overflow, left is truncated and right is preserved.
func formatTwoColumn(left, right string) string {
	const width = 78
	if right == "" {
		return left
	}
	pad := width - displayLen(left) - displayLen(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// displayLen counts runes, not bytes, so multi-byte glyphs (spinner,
// checkmark) line up correctly in the right-aligned column.
func displayLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// humanDuration formats a duration as sub-minute seconds with one
// decimal, longer as "1m 22s".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm %02ds", mins, secs)
}

// SetupPlain is the non-TTY renderer. One line per state transition,
// no cursor games. Safe for pipes, files, and CI capture.
type SetupPlain struct {
	w       Writer
	mu      sync.Mutex
	phases  []setupPhase
	tip     string
	once    sync.Once
	started time.Time
}

// Writer is a tiny alias so SetupPlain can take either an io.Writer or
// a fmt-style writer without pulling fmt's package boundary into the
// signature.
type Writer = io.Writer

// NewSetupPlain constructs the line-by-line renderer. Title is
// printed immediately; the tip (if any) lands at Done so it reads as
// a closing remark.
func NewSetupPlain(w io.Writer, title, tip string, phaseNames []string) *SetupPlain {
	phases := make([]setupPhase, len(phaseNames))
	for i, n := range phaseNames {
		phases[i].name = n
	}
	p := &SetupPlain{w: w, phases: phases, tip: tip, started: time.Now()}
	if title != "" {
		_, _ = fmt.Fprintln(p.w, title)
	}
	return p
}

func (p *SetupPlain) Start(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := findPhase(p.phases, name)
	if idx < 0 {
		return
	}
	if p.phases[idx].state == phaseActive {
		// Re-calling Start for the already-active phase is a no-op;
		// the tap fires many markers per phase and we should not
		// emit a duplicate "-> name" line each time.
		return
	}
	now := time.Now()
	if cur := activePhaseIndex(p.phases); cur >= 0 {
		p.phases[cur].state = phaseDone
		p.phases[cur].endedAt = now
		_, _ = fmt.Fprintf(p.w, "  done in %s\n", humanDuration(p.phases[cur].endedAt.Sub(p.phases[cur].startedAt)))
	}
	// Auto-complete still-pending phases that sit before the new
	// one, matching SetupTTY.Start. Each one prints a "skipped"
	// line so the operator sees the phase happened (or didn't) in
	// order.
	for i := range idx {
		if p.phases[i].state == phasePending {
			p.phases[i].state = phaseDone
			p.phases[i].startedAt = now
			p.phases[i].endedAt = now
			_, _ = fmt.Fprintf(p.w, "  -> %s (skipped, nothing to do)\n", p.phases[i].name)
		}
	}
	p.phases[idx].state = phaseActive
	p.phases[idx].startedAt = now
	_, _ = fmt.Fprintf(p.w, "  -> %s\n", name)
}

func (p *SetupPlain) SetDetail(detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur := activePhaseIndex(p.phases); cur >= 0 {
		if p.phases[cur].detail == detail {
			return
		}
		p.phases[cur].detail = detail
		_, _ = fmt.Fprintf(p.w, "     %s\n", detail)
	}
}

func (p *SetupPlain) Done() {
	p.once.Do(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if cur := activePhaseIndex(p.phases); cur >= 0 {
			p.phases[cur].state = phaseDone
			p.phases[cur].endedAt = time.Now()
			_, _ = fmt.Fprintf(p.w, "  done in %s\n", humanDuration(p.phases[cur].endedAt.Sub(p.phases[cur].startedAt)))
		}
		_, _ = fmt.Fprintf(p.w, "Setup complete in %s.\n", humanDuration(time.Since(p.started)))
		if p.tip != "" {
			_, _ = fmt.Fprintln(p.w, p.tip)
		}
	})
}

func (p *SetupPlain) Fail(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if cur := activePhaseIndex(p.phases); cur >= 0 {
			p.phases[cur].state = phaseFailed
			p.phases[cur].err = err
			_, _ = fmt.Fprintf(p.w, "  failed: %s\n", err.Error())
		}
	})
}
