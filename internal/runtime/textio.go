package runtime

import (
	"bytes"
	"io"
)

// prefixLines writes each complete line of the wrapped stream behind a fixed
// prefix, so a caged server's own stderr reads as "[name] ..." and is never
// mistaken for mcpvessel's output. Partial lines are held until their newline
// arrives; whatever is buffered at teardown is simply dropped with the
// process, which only ever loses a torn final fragment.
type prefixLines struct {
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixLines) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := p.buf[:i+1]
		if _, err := io.WriteString(p.w, p.prefix); err != nil {
			return len(b), err
		}
		if _, err := p.w.Write(line); err != nil {
			return len(b), err
		}
		p.buf = p.buf[i+1:]
	}
}
