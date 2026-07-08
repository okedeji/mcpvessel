package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// TTY renders BuildKit-style live-updating output. It expects a real
// terminal; redirected output keeps the ANSI escapes (use Plain there). A
// ticker goroutine refreshes the dashboard until Done.
type TTY struct {
	w     io.Writer
	mu    sync.Mutex
	state state
	stop  chan struct{}
	once  sync.Once
	width int
}

// state is everything render() needs, guarded by TTY.mu.
type state struct {
	started  time.Time
	total    int
	steps    []ttyStep
	finished bool
	lines    int // lines the previous render emitted
}

type ttyStep struct {
	msg       string
	startedAt time.Time
	endedAt   time.Time
	finished  bool
}

// tickInterval: 100ms keeps the displayed timer smooth without visible CPU.
const tickInterval = 100 * time.Millisecond

// fallbackWidth applies when term.GetSize fails (tests, odd PTY shapes).
const fallbackWidth = 80

// NewTTY starts the refresh loop; Done must be called to stop it.
func NewTTY(w io.Writer) *TTY {
	t := &TTY{
		w:     w,
		state: state{started: time.Now()},
		stop:  make(chan struct{}),
		width: detectWidth(w),
	}
	go t.tickLoop()
	return t
}

func detectWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return fallbackWidth
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil || cols < 40 {
		return fallbackWidth
	}
	return cols
}

func (t *TTY) tickLoop() {
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

func (t *TTY) Step(n, total int, msg string) {
	t.mu.Lock()
	if t.state.total == 0 {
		t.state.total = total
		t.state.steps = make([]ttyStep, total)
	}
	// The previous step is implicitly complete when the next one starts.
	if n > 1 {
		prev := &t.state.steps[n-2]
		if !prev.finished {
			prev.endedAt = time.Now()
			prev.finished = true
		}
	}
	if n >= 1 && n <= len(t.state.steps) {
		t.state.steps[n-1] = ttyStep{msg: msg, startedAt: time.Now()}
	}
	t.mu.Unlock()
	t.render()
}

func (t *TTY) Done() {
	t.once.Do(func() {
		t.mu.Lock()
		if len(t.state.steps) > 0 {
			last := &t.state.steps[len(t.state.steps)-1]
			if !last.finished {
				last.endedAt = time.Now()
				last.finished = true
			}
		}
		t.state.finished = true
		t.mu.Unlock()
		close(t.stop)
		t.render()
	})
}

func (t *TTY) render() {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Move the cursor back to the top of the previous render so the new
	// lines overwrite it in place rather than scrolling.
	if t.state.lines > 0 {
		_, _ = fmt.Fprintf(t.w, "\033[%dA", t.state.lines)
	}

	elapsed := time.Since(t.state.started)
	done := 0
	for _, s := range t.state.steps {
		if s.finished {
			done++
		}
	}
	stateLabel := "RUNNING"
	if t.state.finished {
		stateLabel = "FINISHED"
	}
	header := fmt.Sprintf("[+] Building %.1fs (%d/%d) %s",
		elapsed.Seconds(), done, t.state.total, stateLabel)
	t.writeLine(header)

	for i, s := range t.state.steps {
		t.writeStep(i+1, t.state.total, s)
	}

	t.state.lines = 1 + len(t.state.steps)
}

func (t *TTY) writeStep(n, total int, s ttyStep) {
	var dur time.Duration
	if s.finished {
		dur = s.endedAt.Sub(s.startedAt)
	} else if !s.startedAt.IsZero() {
		dur = time.Since(s.startedAt)
	}
	prefix := fmt.Sprintf(" => [%d/%d] %s", n, total, s.msg)
	duration := fmt.Sprintf("%.1fs", dur.Seconds())
	t.writeLineWithRight(prefix, duration)
}

// writeLine truncates to terminal width and clears to end of line so a
// previous, longer render does not bleed through.
func (t *TTY) writeLine(s string) {
	if len(s) > t.width {
		s = s[:t.width]
	}
	_, _ = fmt.Fprintf(t.w, "%s\033[K\n", s)
}

// writeLineWithRight emits a left-aligned prefix and a right-aligned
// trailer on one line, padded to terminal width.
func (t *TTY) writeLineWithRight(left, right string) {
	pad := t.width - len(left) - len(right)
	if pad < 1 {
		pad = 1
		// Truncate left so right can still fit.
		max := t.width - len(right) - 1
		if max < 0 {
			max = 0
		}
		if len(left) > max {
			left = left[:max]
		}
	}
	_, _ = fmt.Fprintf(t.w, "%s%s%s\033[K\n", left, strings.Repeat(" ", pad), right)
}
