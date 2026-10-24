package output

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func samplePanel() Panel {
	return Panel{Title: "Getting started", Entries: []PanelEntry{
		{Name: "setup", Desc: "Complete login, organization, agent, and daemon onboarding"},
		{Name: "doctor", Desc: "Diagnose configuration"},
		{Name: "--no-interactive", Desc: "Disable prompts and browser interaction"},
	}}
}

// Every line of a panel must occupy exactly the requested width, or the box
// will not close. This is the property that styling most easily breaks, since
// escape sequences occupy no terminal cells.
func TestPanelLinesAreExactlyTheRequestedWidth(t *testing.T) {
	for _, styled := range []bool{false, true} {
		var out bytes.Buffer
		r := Renderer{Mode: Human, Out: &out, OutInteractive: styled}
		if err := r.Panel(&out, samplePanel(), 76); err != nil {
			t.Fatal(err)
		}
		for index, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
			if got := cellWidth(stripANSI(line)); got != 76 {
				t.Errorf("styled=%t line %d width = %d, want 76: %q", styled, index, got, stripANSI(line))
			}
		}
	}
}

func TestPanelDrawsATitledBox(t *testing.T) {
	// Box drawing depends on the terminal, not on the test host. Windows CI runs
	// without WT_SESSION, where the ASCII fallback is the correct output, so the
	// modern-terminal condition is stated explicitly rather than assumed. Both
	// inputs are pinned so the test cannot be swayed by the caller's environment.
	t.Setenv("BEAM_ASCII_BOX", "")
	t.Setenv("WT_SESSION", "1")
	var out bytes.Buffer
	r := Renderer{Mode: Human, Out: &out}
	if err := r.Panel(&out, samplePanel(), 60); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "╭─ Getting started ─") {
		t.Errorf("first line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "╰─") || !strings.HasSuffix(lines[len(lines)-1], "╯") {
		t.Errorf("last line = %q", lines[len(lines)-1])
	}
	// Entry names are padded to a common column so descriptions line up.
	if !strings.Contains(lines[1], "setup             Complete") {
		t.Errorf("entry line = %q", lines[1])
	}
}

// A description longer than the box must be truncated, never wrapped: a panel
// stays exactly len(entries)+2 lines tall so sections keep a predictable shape.
func TestPanelTruncatesRatherThanWraps(t *testing.T) {
	var out bytes.Buffer
	r := Renderer{Mode: Human, Out: &out}
	long := Panel{Title: "T", Entries: []PanelEntry{{Name: "x", Desc: strings.Repeat("long ", 40)}}}
	if err := r.Panel(&out, long, 50); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("panel is %d lines, want 3", len(lines))
	}
	if !strings.Contains(lines[1], "…") {
		t.Errorf("long description was not truncated: %q", lines[1])
	}
}

func TestPanelIsUnstyledWhenOutIsNotInteractive(t *testing.T) {
	var out bytes.Buffer
	r := Renderer{Mode: Human, Out: &out, OutInteractive: false}
	if err := r.Panel(&out, samplePanel(), 60); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("piped panel contains escape sequences: %q", out.String())
	}
}

func TestPanelHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	r := Renderer{Mode: Human, Out: &out, OutInteractive: true}
	if err := r.Panel(&out, samplePanel(), 60); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("NO_COLOR panel contains escape sequences: %q", out.String())
	}
}

func TestPanelFallsBackToASCIIBox(t *testing.T) {
	t.Setenv("BEAM_ASCII_BOX", "1")
	var out bytes.Buffer
	r := Renderer{Mode: Human, Out: &out}
	if err := r.Panel(&out, samplePanel(), 60); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "╭╮╰╯─│") {
		t.Errorf("ASCII fallback still drew box characters: %q", out.String())
	}
	if !strings.HasPrefix(out.String(), "+- Getting started") {
		t.Errorf("ASCII fallback = %q", strings.SplitN(out.String(), "\n", 2)[0])
	}
}

// A non-terminal writer reports no width, which is how callers know to render
// plain text instead of a box.
func TestTerminalWidthIsZeroForANonTerminal(t *testing.T) {
	if got := TerminalWidth(&bytes.Buffer{}); got != 0 {
		t.Errorf("TerminalWidth(buffer) = %d, want 0", got)
	}
}

func TestBoxStyleHonorsTheExplicitOverride(t *testing.T) {
	t.Setenv("WT_SESSION", "1")
	t.Setenv("BEAM_ASCII_BOX", "1")
	if boxStyle() != asciiBox {
		t.Error("BEAM_ASCII_BOX did not force the ASCII fallback")
	}
}

// The legacy Windows console cannot be relied on for box drawing, and is the
// only platform difference in this package. A macOS or Linux developer never
// sees it, so it is pinned here rather than left to CI to discover.
func TestBoxStyleFallsBackOnLegacyWindowsConsole(t *testing.T) {
	t.Setenv("BEAM_ASCII_BOX", "")
	t.Setenv("WT_SESSION", "")
	want := roundedBox
	if runtime.GOOS == "windows" {
		want = asciiBox
	}
	if got := boxStyle(); got != want {
		t.Errorf("boxStyle() on %s = %+v, want %+v", runtime.GOOS, got, want)
	}
}

// Windows Terminal reports itself, and does support box drawing.
func TestBoxStyleUsesRoundedInWindowsTerminal(t *testing.T) {
	t.Setenv("BEAM_ASCII_BOX", "")
	t.Setenv("WT_SESSION", "1")
	if boxStyle() != roundedBox {
		t.Error("a modern terminal should get rounded box drawing")
	}
}
