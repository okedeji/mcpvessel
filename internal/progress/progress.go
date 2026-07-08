// Package progress renders build progress: Plain (line-by-line, safe
// everywhere) and TTY (BuildKit-style live dashboard), selected by the
// --progress flag.
package progress

import (
	"io"
	"os"

	"golang.org/x/term"
)

// Renderer reports build progress to its writer.
type Renderer interface {
	// Step reports that step n of total has started; the previous step is
	// implicitly complete.
	Step(n, total int, msg string)
	// Done finalizes rendering and stops any tick goroutine. Step calls
	// after Done are undefined.
	Done()
}

// Mode selects which renderer to construct.
type Mode string

const (
	ModeAuto  Mode = "auto"
	ModePlain Mode = "plain"
	ModeTTY   Mode = "tty" // live renderer even when piped
)

// ParseMode normalizes a flag value into a Mode; anything unrecognized
// becomes ModeAuto rather than an error.
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

// IsTerminal reports whether w is an interactive terminal: false for
// pipes, regular files, and non-*os.File writers.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
