// Package cliout is the single output vocabulary the CLI speaks: one table
// shape and one empty-state shape, so every list command renders the way ps
// and store ls do rather than each hand-rolling its own spacing.
package cliout

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Table renders rows under uppercase headers with three-space gutters, the
// shape ps and store ls set. A cell with nothing to say renders "-" so a gap
// never reads as a missing column.
func Table(w io.Writer, headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			if c == "" {
				c = "-"
			}
			cells[i] = c
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

// Empty prints a list command's empty state: one sentence saying what empty
// means and, when there is one, the command that fills it. A list command
// prints this instead of a bare header row or silence, so an empty result
// never reads as a broken command.
func Empty(w io.Writer, msg string) {
	_, _ = fmt.Fprintln(w, msg)
}

// Note prints a short line under a table: what the rows do not say on their own,
// and what to do about it. Kept blank-line separated so it reads as commentary
// on the table rather than another row.
func Note(w io.Writer, msg string) {
	_, _ = fmt.Fprintf(w, "\nnote: %s\n", msg)
}
