package output

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Style names the meaning of a cell rather than a color, so the palette can
// change in one place and callers keep describing their data.
type Style string

const (
	StyleNone Style = ""
	// StyleID marks an identifier the reader is likely to copy into the next
	// command.
	StyleID Style = ansiCyan
	// StyleMuted marks a value that is present but not what the reader is
	// scanning for.
	StyleMuted Style = ansiDim
	// StyleOK, StyleAttention, and StyleProblem describe a lifecycle state.
	StyleOK        Style = ansiGreen
	StyleAttention Style = ansiYellow
	StyleProblem   Style = ansiRed
)

// columnGap separates columns. Two spaces read as a single break at any column
// width, where one space lets long values run together.
const columnGap = "  "

// Table is a batch listing whose rows are all known before the first is
// printed. That is what makes alignment possible: column widths are measured
// across every row, so a short identifier in one row cannot shift the columns
// of another.
//
// Streaming output must not use this. A watch or log stream emits each row as
// its event arrives, and buffering rows to measure them would trade live output
// for tidy columns.
type Table struct {
	Headers []string
	Rows    [][]string
	// Style optionally reports how one cell should be rendered. It is consulted
	// only when the result stream is an interactive terminal, so the returned
	// styles never reach a pipe or a file.
	Style func(column int, value string) Style
}

// Table renders a batch listing on w. Styling follows OutInteractive, which is
// false whenever stdout is piped or redirected, so a script reading this
// command sees plain text with aligned columns and no escape sequences.
func (r Renderer) Table(w io.Writer, table Table) error {
	return WriteTable(w, table, r.OutInteractive)
}

// WriteTable renders a batch listing for a caller that holds no Renderer.
// styled carries the same meaning as Renderer.OutInteractive: it must be true
// only when w is an interactive terminal. NO_COLOR is honored here, so no
// caller has to remember it.
func WriteTable(w io.Writer, table Table, styled bool) error {
	// A header with no rows under it is noise, and it would also turn today's
	// silent empty listing into output that a caller has to special-case. An
	// empty table prints nothing.
	if len(table.Rows) == 0 {
		return nil
	}
	styled = styled && colorEnabled()
	widths := make([]int, len(table.Headers))
	for index, header := range table.Headers {
		widths[index] = cellWidth(header)
	}
	for _, row := range table.Rows {
		for index, value := range row {
			if index < len(widths) && cellWidth(value) > widths[index] {
				widths[index] = cellWidth(value)
			}
		}
	}

	if len(table.Headers) > 0 {
		header := padRow(table.Headers, widths, nil)
		if styled {
			// The header names the columns once; it is not the data, so it recedes.
			header = string(ansiDim) + header + ansiReset
		}
		if _, err := fmt.Fprintln(w, header); err != nil {
			return err
		}
	}

	for _, row := range table.Rows {
		style := table.Style
		if !styled {
			style = nil
		}
		if _, err := fmt.Fprintln(w, padRow(row, widths, style)); err != nil {
			return err
		}
	}
	return nil
}

// padRow pads each cell to its column width and joins them. Padding is added
// outside the escape sequences so a styled cell occupies the same number of
// terminal cells as an unstyled one.
func padRow(row []string, widths []int, style func(column int, value string) Style) string {
	cells := make([]string, 0, len(row))
	for index, value := range row {
		width := 0
		if index < len(widths) {
			width = widths[index]
		}
		padding := strings.Repeat(" ", max(width-cellWidth(value), 0))
		if style != nil {
			if applied := style(index, value); applied != StyleNone {
				value = string(applied) + value + ansiReset
			}
		}
		cells = append(cells, value+padding)
	}
	// Trailing padding on the last column is invisible but would show up in a
	// diff or a golden test, so it is removed.
	return strings.TrimRight(strings.Join(cells, columnGap), " ")
}

// cellWidth counts runes rather than bytes. Beam renders identifiers, states,
// and URLs, which are ASCII; a full width-aware measurement would only matter
// for CJK or emoji values that these listings do not carry.
func cellWidth(value string) int {
	return utf8.RuneCountInString(value)
}

// StateStyle colors a lifecycle value by what the reader should do about it,
// not by which subsystem produced it. Rooms, endpoints, operations, and
// transfers all report states from an overlapping vocabulary, so one mapping
// keeps "active" the same color everywhere it appears.
//
// An unrecognized value is left unstyled rather than guessed at. States are
// defined by the coordinator, so this list will always be incomplete, and a
// wrong color is worse than none.
func StateStyle(value string) Style {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "active", "ready", "running", "connected", "open", "completed", "succeeded", "delivered", "published", "valid", "online":
		return StyleOK
	case "pending", "suspended", "in_progress", "queued", "starting", "stopping", "degraded", "retrying":
		return StyleAttention
	case "failed", "error", "denied", "invalid", "unauthorized", "timeout":
		return StyleProblem
	case "closed", "cancelled", "canceled", "revoked", "expired", "consumed", "stopped", "removed", "inactive", "offline", "-":
		return StyleMuted
	default:
		return StyleNone
	}
}

// RowStyle is the styling most listings want: the first column is the
// identifier a reader copies into the next command, and one column holds a
// lifecycle state. Pass a negative stateColumn when the listing has no state.
func RowStyle(stateColumn int) func(column int, value string) Style {
	return func(column int, value string) Style {
		switch column {
		case 0:
			return StyleID
		case stateColumn:
			return StateStyle(value)
		default:
			return StyleNone
		}
	}
}
