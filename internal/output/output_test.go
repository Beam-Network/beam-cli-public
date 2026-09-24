package output

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestJSONResultContainsNoExtraText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := Renderer{Mode: JSON, Out: &stdout, Err: &stderr}
	if err := r.Result(map[string]any{"ok": true}, nil); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if got["ok"] != true || stderr.Len() != 0 {
		t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestJSONErrorIsStructured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := Renderer{Mode: JSON, Out: &stdout, Err: &stderr}
	r.Error(6, "Daemon unavailable.", "Start beam-agent.")
	var got map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestJSONErrorIncludesStableKind(t *testing.T) {
	var stderr bytes.Buffer
	r := Renderer{Mode: JSON, Out: io.Discard, Err: &stderr}
	r.ErrorWithKind(8, "organization_required", "No organization.", "Create one.")
	var got struct {
		Error struct {
			Code int    `json:"code"`
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != 8 || got.Error.Kind != "organization_required" {
		t.Fatalf("error = %+v", got.Error)
	}
}

func TestInteractiveHyperlinkUsesOSC8(t *testing.T) {
	r := Renderer{Mode: Human, Interactive: true}
	rawURL := "https://auth.beam.network/connect?code=BEAM"
	link := r.Hyperlink(rawURL)
	if !strings.Contains(link, "\x1b]8;;https://auth.beam.network/") {
		t.Fatalf("link is not an OSC 8 hyperlink: %q", link)
	}
	if !strings.Contains(link, "\x1b\\"+rawURL+"\x1b]8;;") {
		t.Fatalf("full URL is not visible in link: %q", link)
	}
}

func TestHyperlinkRejectsTerminalEscapeInjection(t *testing.T) {
	r := Renderer{Mode: Human, Interactive: true}
	raw := "https://auth.beam.network/\x1b]8;;https://evil.example"
	if got := r.Hyperlink(raw); got != raw {
		t.Fatalf("unsafe URL should not become a hyperlink: %q", got)
	}
}

func TestHighlightURLStylesInteractiveOutput(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	r := Renderer{Mode: Human, OutInteractive: true}
	rawURL := "https://ndmq7g.tunnel.b3m.dev"
	styled := r.HighlightURL(rawURL)
	if !strings.Contains(styled, "\x1b]8;;"+rawURL+"\x1b\\") {
		t.Fatalf("URL is not a clickable OSC 8 hyperlink: %q", styled)
	}
	if !strings.Contains(styled, ansiBold) || !strings.Contains(styled, ansiCyan) {
		t.Fatalf("URL is not colored/bold: %q", styled)
	}
}

func TestHighlightURLStaysRawWhenPiped(t *testing.T) {
	r := Renderer{Mode: Human, OutInteractive: false}
	rawURL := "https://ndmq7g.tunnel.b3m.dev"
	if got := r.HighlightURL(rawURL); got != rawURL {
		t.Fatalf("piped output must stay raw for scripts: %q", got)
	}
}

func TestHighlightURLHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	r := Renderer{Mode: Human, OutInteractive: true}
	rawURL := "https://ndmq7g.tunnel.b3m.dev"
	styled := r.HighlightURL(rawURL)
	if strings.Contains(styled, ansiCyan) || strings.Contains(styled, ansiBold) {
		t.Fatalf("NO_COLOR must drop color codes: %q", styled)
	}
	if !strings.Contains(styled, "\x1b]8;;"+rawURL) {
		t.Fatalf("hyperlink should remain even without color: %q", styled)
	}
}

func TestNonInteractiveSpinnerWritesOneStatusLine(t *testing.T) {
	var stderr bytes.Buffer
	r := Renderer{Mode: Human, Err: &stderr}
	spinner := r.StartSpinner("Waiting for authorization")
	spinner.Stop()
	if got, want := stderr.String(), "Waiting for authorization...\n"; got != want {
		t.Fatalf("stderr=%q want=%q", got, want)
	}
}

func TestJSONSpinnerIsSilent(t *testing.T) {
	var stderr bytes.Buffer
	r := Renderer{Mode: JSON, Err: &stderr, Interactive: true}
	spinner := r.StartSpinner("Waiting for authorization")
	spinner.Stop()
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestSpinnerMessageCanBeUpdated(t *testing.T) {
	var stderr bytes.Buffer
	r := Renderer{Mode: Human, Err: &stderr}
	spinner := r.StartSpinner("Preparing tunnel")
	spinner.Update("Finalizing DNS and TLS")
	if got := spinner.Message(); got != "Finalizing DNS and TLS" {
		t.Fatalf("message=%q", got)
	}
	spinner.Stop()
}

func TestTableAlignsColumnsAcrossRows(t *testing.T) {
	var stdout bytes.Buffer
	r := Renderer{Mode: Human, Out: &stdout}
	err := r.Table(&stdout, Table{
		Headers: []string{"ROOM ID", "STATE", "MEMBER"},
		Rows: [][]string{
			{"btr_room_jqoxvom2eajf27y23dvjiweq7i", "active", "btr_member_a"},
			{"btr_room_a", "suspended", "btr_member_bbbb"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"ROOM ID                              STATE      MEMBER",
		"btr_room_jqoxvom2eajf27y23dvjiweq7i  active     btr_member_a",
		"btr_room_a                           suspended  btr_member_bbbb",
		"",
	}, "\n")
	if stdout.String() != want {
		t.Errorf("table =\n%q\nwant\n%q", stdout.String(), want)
	}
}

// A piped stdout is the case that matters most: every existing script reading
// these listings must keep seeing plain text.
func TestTableIsUnstyledWhenOutIsNotInteractive(t *testing.T) {
	var stdout bytes.Buffer
	r := Renderer{Mode: Human, Out: &stdout, OutInteractive: false}
	err := r.Table(&stdout, Table{
		Headers: []string{"ID", "STATE"},
		Rows:    [][]string{{"room-a", "active"}},
		Style:   func(int, string) Style { return StyleID },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "\x1b") {
		t.Errorf("piped table contains escape sequences: %q", stdout.String())
	}
}

func TestTableHonorsNoColorOnInteractiveOut(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var stdout bytes.Buffer
	r := Renderer{Mode: Human, Out: &stdout, OutInteractive: true}
	err := r.Table(&stdout, Table{
		Headers: []string{"ID"},
		Rows:    [][]string{{"room-a"}},
		Style:   func(int, string) Style { return StyleProblem },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "\x1b") {
		t.Errorf("NO_COLOR table contains escape sequences: %q", stdout.String())
	}
}

// Styling must not disturb alignment: the escape sequences occupy no terminal
// cells, so padding is computed on the visible text and added outside them.
func TestTableStylingPreservesAlignment(t *testing.T) {
	var stdout bytes.Buffer
	r := Renderer{Mode: Human, Out: &stdout, OutInteractive: true}
	err := r.Table(&stdout, Table{
		Headers: []string{"ID", "STATE"},
		Rows:    [][]string{{"a", "active"}, {"bbbb", "closed"}},
		Style: func(column int, _ string) Style {
			if column == 0 {
				return StyleID
			}
			return StyleNone
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "\x1b") {
		t.Fatal("interactive table should be styled")
	}
	for _, line := range strings.Split(strings.TrimRight(stripANSI(stdout.String()), "\n"), "\n") {
		if !strings.HasPrefix(line[6:], "STATE") && !strings.HasPrefix(line[6:], "active") && !strings.HasPrefix(line[6:], "closed") {
			t.Errorf("column 1 is misaligned in %q", line)
		}
	}
}

func TestTableTrimsTrailingPadding(t *testing.T) {
	var stdout bytes.Buffer
	r := Renderer{Mode: Human, Out: &stdout}
	err := r.Table(&stdout, Table{
		Headers: []string{"ID", "URL"},
		Rows:    [][]string{{"a", "https://example.test/very/long"}, {"b", "-"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("line has trailing padding: %q", line)
		}
	}
}

func stripANSI(text string) string {
	var builder strings.Builder
	for {
		start := strings.Index(text, "\x1b[")
		if start < 0 {
			builder.WriteString(text)
			return builder.String()
		}
		builder.WriteString(text[:start])
		end := strings.IndexByte(text[start:], 'm')
		if end < 0 {
			return builder.String()
		}
		text = text[start+end+1:]
	}
}

// Errors go to stderr, so their styling follows Interactive, not OutInteractive.
// A command whose stdout is piped still deserves a readable error on the
// terminal it was launched from.
func TestErrorLabelsFollowTheStatusStream(t *testing.T) {
	for _, test := range []struct {
		name        string
		interactive bool
		outInteract bool
		wantStyled  bool
	}{
		{name: "terminal stderr is styled", interactive: true, outInteract: false, wantStyled: true},
		{name: "piped stderr is plain", interactive: false, outInteract: true, wantStyled: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			r := Renderer{Mode: Human, Out: io.Discard, Err: &stderr, Interactive: test.interactive, OutInteractive: test.outInteract}
			r.Error(8, "Something failed.", "Try again.")
			got := strings.Contains(stderr.String(), "\x1b")
			if got != test.wantStyled {
				t.Errorf("styled = %t, want %t: %q", got, test.wantStyled, stderr.String())
			}
			// The message itself is never styled, only the label.
			if !strings.Contains(stripANSI(stderr.String()), "Error: Something failed.") {
				t.Errorf("stderr = %q", stripANSI(stderr.String()))
			}
		})
	}
}

func TestErrorLabelsHonorNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var stderr bytes.Buffer
	r := Renderer{Mode: Human, Out: io.Discard, Err: &stderr, Interactive: true}
	r.Error(8, "Something failed.", "Try again.")
	if strings.Contains(stderr.String(), "\x1b") {
		t.Errorf("NO_COLOR error contains escape sequences: %q", stderr.String())
	}
}
