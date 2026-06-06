package progress

import (
	"fmt"
	"io"
)

// Plain is the Docker classic builder format: one line per step start,
// nothing more. Safe to use anywhere (CI logs, piped output, files).
type Plain struct {
	w io.Writer
}

// NewPlain constructs a Plain renderer writing to w.
func NewPlain(w io.Writer) *Plain {
	return &Plain{w: w}
}

func (p *Plain) Step(n, total int, msg string) {
	_, _ = fmt.Fprintf(p.w, "Step %d/%d : %s\n", n, total, msg)
}

func (p *Plain) Done() {}
