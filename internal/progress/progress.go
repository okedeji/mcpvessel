// Package progress renders build progress to a writer. Two renderers
// ship: Plain (Docker classic line-by-line output, safe everywhere) and
// TTY (BuildKit-style live-updating dashboard with cursor control). The
// CLI picks one based on a --progress flag with the same semantics as
// docker build's --progress.
package progress

import (
	"io"
	"os"

	"golang.org/x/term"
)

// Renderer reports build progress to its writer.
type Renderer interface {
	// Step reports that step n of total has started with the given
	// message. The renderer infers the previous step's completion from
	// the next Step call.
	Step(n, total int, msg string)
	// Done finalizes rendering and releases resources held by the
	// renderer (timer goroutines, terminal state). Safe to call once;
	// further Step calls are undefined after Done.
	Done()
}

// Mode selects which renderer to construct.
type Mode string

const (
	// ModeAuto picks Plain when the writer isn't a TTY and TTY otherwise.
	ModeAuto Mode = "auto"
	// ModePlain forces line-by-line output.
	ModePlain Mode = "plain"
	// ModeTTY forces the live-updating renderer, even when piped.
	ModeTTY Mode = "tty"
)

// ParseMode normalizes a flag value into a Mode. Empty strings and
// unrecognized values become ModeAuto so users can't accidentally
// degrade their experience by typoing the flag.
func ParseMode(s string) Mode {
	switch Mode(s) {
	case ModePlain, ModeTTY, ModeAuto:
		return Mode(s)
	default:
		return ModeAuto
	}
}

// New returns a renderer for the given mode, writing to w.
func New(w io.Writer, mode Mode) Renderer {
	switch mode {
	case ModePlain:
		return NewPlain(w)
	case ModeTTY:
		return NewTTY(w)
	default:
		if IsTerminal(w) {
			return NewTTY(w)
		}
		return NewPlain(w)
	}
}

// IsTerminal reports whether w is a real interactive terminal. Returns
// false for anything that isn't an *os.File or whose file descriptor is
// not a TTY (pipes, regular files, CI capture buffers).
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
