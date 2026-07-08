package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Setup renders a multi-phase first-time-setup flow with the phases fixed
// up front. It exists to replace Lima's raw output, which a first-time user
// cannot interpret; --verbose bypasses it for the raw stream.
type Setup interface {
	// Start marks the named phase active, completing the previously
	// active one. An unknown name is a no-op, never a failure.
	Start(name string)
	// SetDetail attaches a short sub-status to the active phase; empty
	// clears it.
	SetDetail(detail string)
	// Done completes the active phase and stops the renderer. Safe to
	// call more than once.
	Done()
	// Fail marks the active phase failed and stops the renderer.
	Fail(err error)
}

// NewSetup picks SetupTTY when w is a terminal, SetupPlain otherwise.
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

// SetupTTY is the live-updating renderer; a tick goroutine refreshes
// elapsed times until Done.
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

// NewSetupTTY renders the header immediately so the user sees something
// before the first Start.
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
	// The Lima tap fires Start once per marker and a phase has many
	// markers; without this guard each one would reset startedAt and
	// clear detail, pinning the UI at "0s".
	if t.phases[idx].state == phaseActive {
		return
	}
	now := time.Now()
	// Complete the active phase, then auto-complete pending phases before
	// the new one: the tap can miss a marker, and a cached image gives a
	// phase nothing to do. Otherwise skipped phases keep their dot forever.
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

// spinnerFrames is the usual Braille spinner, indexed by elapsed
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

// writeLine appends s plus the clear-to-end-of-line escape so a previous,
// longer render does not bleed through.
func writeLine(b *strings.Builder, s string) {
	b.WriteString(s)
	b.WriteString("\033[K\n")
}

// formatSetupPhase renders one phase line:
//
//	✓ <name>                                  3s
//	⠼ <name> - <detail>                  1m 22s
//	· <name>
//	✗ <name> - <error>                  (failed)
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

// formatTwoColumn pads so right ends at column 78; on overflow left is
// truncated and right preserved.
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

// displayLen counts runes so multi-byte glyphs align.
func displayLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm %02ds", mins, secs)
}

// SetupPlain is the non-TTY renderer: one line per transition, no cursor
// control.
type SetupPlain struct {
	w       Writer
	mu      sync.Mutex
	phases  []setupPhase
	tip     string
	once    sync.Once
	started time.Time
}

// Writer aliases io.Writer.
type Writer = io.Writer

// NewSetupPlain prints the title immediately; the tip, if any, lands at
// Done.
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
		// The tap fires many markers per phase; one "-> name" line each.
		return
	}
	now := time.Now()
	if cur := activePhaseIndex(p.phases); cur >= 0 {
		p.phases[cur].state = phaseDone
		p.phases[cur].endedAt = now
		_, _ = fmt.Fprintf(p.w, "  done in %s\n", humanDuration(p.phases[cur].endedAt.Sub(p.phases[cur].startedAt)))
	}
	// Auto-complete pending phases before the new one, matching
	// SetupTTY.Start; each prints a skipped line so order stays visible.
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
