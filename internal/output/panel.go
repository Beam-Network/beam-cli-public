package output

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// PanelEntry is one labelled row inside a panel: a name the reader will type,
// and what it does.
type PanelEntry struct {
	Name string
	Desc string
}

// Panel is a titled group of entries drawn inside a box. It exists to give a
// long command or option list visible structure, which a flat indented list
// cannot: at 26 entries the reader has nothing to scan by.
//
// A panel is only ever drawn to an interactive terminal. Piped output keeps the
// plain indented form, so `beam --help | grep room` and the help text embedded
// in --json stay free of box drawing.
type Panel struct {
	Title   string
	Entries []PanelEntry
}

const (
	// panelMaxWidth stops descriptions drifting far from their names on a wide
	// window. Beyond roughly this width the eye loses the row.
	panelMaxWidth = 92
	// panelMinWidth is the point below which a box costs more room than the
	// structure it adds; narrower terminals fall back to plain output.
	panelMinWidth = 40
)

type boxChars struct{ topLeft, topRight, bottomLeft, bottomRight, horizontal, vertical string }

var (
	roundedBox = boxChars{"╭", "╮", "╰", "╯", "─", "│"}
	asciiBox   = boxChars{"+", "+", "+", "+", "-", "|"}
)

// boxStyle picks the box characters. Box drawing is reliable on modern
// terminals but not on the legacy Windows console, which is detected here by
// the absence of the Windows Terminal session variable. BEAM_ASCII_BOX forces
// the fallback anywhere.
func boxStyle() boxChars {
	if os.Getenv("BEAM_ASCII_BOX") != "" {
		return asciiBox
	}
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" {
		return asciiBox
	}
	return roundedBox
}

// TerminalWidth reports the usable width for w, clamped to the range a panel
// can be drawn in. It returns 0 when w is not a terminal, which is the caller's
// signal to render plain text instead.
func TerminalWidth(w io.Writer) int {
	file, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 {
		// A terminal that will not report its size still deserves structure, so
		// fall back to COLUMNS and then to the conventional 80.
		width = 80
		if raw := os.Getenv("COLUMNS"); raw != "" {
			if parsed, convErr := strconv.Atoi(raw); convErr == nil && parsed > 0 {
				width = parsed
			}
		}
	}
	if width > panelMaxWidth {
		width = panelMaxWidth
	}
	if width < panelMinWidth {
		return 0
	}
	return width
}

// Panel draws one boxed section on w at the given width. Callers obtain width
// from TerminalWidth and skip panels entirely when it reports 0.
func (r Renderer) Panel(w io.Writer, panel Panel, width int) error {
	box := boxStyle()
	styled := r.OutInteractive && colorEnabled()
	inner := width - 2

	title := panel.Title
	if styled {
		title = ansiBold + title + ansiReset
	}
	// The rule after the title fills whatever the title left, measured on the
	// visible text so styling cannot change the box width.
	rule := inner - cellWidth(panel.Title) - 3
	if rule < 0 {
		rule = 0
	}
	if _, err := fmt.Fprintln(w, paint(styled, ansiDim, box.topLeft+box.horizontal+" ")+title+paint(styled, ansiDim, " "+strings.Repeat(box.horizontal, rule)+box.topRight)); err != nil {
		return err
	}

	nameWidth := 0
	for _, entry := range panel.Entries {
		if cellWidth(entry.Name) > nameWidth {
			nameWidth = cellWidth(entry.Name)
		}
	}

	for _, entry := range panel.Entries {
		// Two leading spaces, the name column, two separating spaces, then the
		// description, with one trailing space before the right edge.
		room := inner - nameWidth - 5
		desc := entry.Desc
		if room > 1 && cellWidth(desc) > room {
			desc = string([]rune(desc)[:room-1]) + "…"
		}
		name := entry.Name + strings.Repeat(" ", nameWidth-cellWidth(entry.Name))
		trailing := inner - nameWidth - 4 - cellWidth(desc)
		if trailing < 0 {
			trailing = 0
		}
		line := paint(styled, ansiDim, box.vertical) + "  " + paint(styled, ansiCyan, name) + "  " +
			desc + strings.Repeat(" ", trailing) + paint(styled, ansiDim, box.vertical)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintln(w, paint(styled, ansiDim, box.bottomLeft+strings.Repeat(box.horizontal, inner)+box.bottomRight))
	return err
}

func paint(styled bool, style, text string) string {
	if !styled {
		return text
	}
	return style + text + ansiReset
}
