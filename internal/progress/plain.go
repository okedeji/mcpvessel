package progress

import (
	"fmt"
	"io"
)

// Plain prints one line per step start, nothing more. Safe anywhere.
type Plain struct {
	w io.Writer
}

func NewPlain(w io.Writer) *Plain {
	return &Plain{w: w}
}

func (p *Plain) Step(n, total int, msg string) {
	_, _ = fmt.Fprintf(p.w, "Step %d/%d : %s\n", n, total, msg)
}

func (p *Plain) Done() {}
