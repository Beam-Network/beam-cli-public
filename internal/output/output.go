package output

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Mode string

const (
	Human Mode = "human"
	JSON  Mode = "json"
	Quiet Mode = "quiet"
)

type Renderer struct {
	Mode Mode
	Out  io.Writer
	Err  io.Writer
	// Interactive reports whether the status channel (Err) is an interactive
	// terminal, gating spinners and stderr hyperlinks.
	Interactive bool
	// OutInteractive reports whether the result channel (Out) is an interactive
	// terminal. It stays false when Out is piped or redirected so scripts capture
	// exact, unstyled values.
	OutInteractive bool
	Verbose        bool
}

const (
	ansiReset     = "\x1b[0m"
	ansiBold      = "\x1b[1m"
	ansiDim       = "\x1b[2m"
	ansiUnderline = "\x1b[4m"
	ansiCyan      = "\x1b[38;5;45m"
	ansiGreen     = "\x1b[38;5;78m"
	ansiYellow    = "\x1b[38;5;179m"
	ansiRed       = "\x1b[38;5;203m"
)

func (r Renderer) Result(value any, human func(io.Writer) error) error {
	switch r.Mode {
	case Quiet:
		return nil
	case JSON:
		encoder := json.NewEncoder(r.Out)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(value)
	default:
		if human != nil {
			return human(r.Out)
		}
		_, err := fmt.Fprintln(r.Out, value)
		return err
	}
}

func (r Renderer) Error(code int, message, hint string) {
	r.ErrorWithKind(code, "", message, hint)
}

func (r Renderer) ErrorWithKind(code int, kind, message, hint string) {
	if r.Mode == JSON {
		payload := map[string]any{"code": code, "message": message}
		if kind != "" {
			payload["kind"] = kind
		}
		if hint != "" {
			payload["hint"] = hint
		}
		_ = json.NewEncoder(r.Err).Encode(map[string]any{"error": payload})
		return
	}
	// Only the labels are styled. The message is the part the reader has to
	// read word by word, and coloring it makes it harder, not easier; the label
	// is what tells them at a glance which stream they are looking at.
	_, _ = fmt.Fprintf(r.Err, "%s %s\n", r.errLabel(ansiRed, "Error:"), message)
	if hint != "" {
		_, _ = fmt.Fprintf(r.Err, "%s %s\n", r.errLabel(ansiYellow, "Hint:"), hint)
	}
}

// errLabel styles a label on the status stream, which is gated by Interactive
// rather than OutInteractive: errors go to stderr, so a command whose stdout is
// piped still deserves a readable error on the terminal.
func (r Renderer) errLabel(style, text string) string {
	if !r.Interactive || !colorEnabled() {
		return text
	}
	return style + ansiBold + text + ansiReset
}

// Label styles a heading on the result stream.
func (r Renderer) Label(text string) string {
	if !r.OutInteractive || !colorEnabled() {
		return text
	}
	return ansiBold + text + ansiReset
}

func (r Renderer) Progress(message string) {
	if r.Mode == Human {
		_, _ = fmt.Fprintln(r.Err, message)
	}
}

// Hyperlink emits an OSC 8 terminal hyperlink whose visible text remains the
// URL. This lets terminals without OSC 8 support detect the plain URL.
func (r Renderer) Hyperlink(rawURL string) string {
	return r.Link(rawURL, rawURL)
}

// Link emits an OSC 8 terminal hyperlink with a concise visible label.
func (r Renderer) Link(label, rawURL string) string {
	if !r.Interactive || !safeHTTPURL(rawURL) {
		return rawURL
	}
	return "\x1b]8;;" + rawURL + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

// HighlightURL renders a result URL so it stands out on an interactive terminal:
// bold, colored, underlined, and clickable via OSC 8. When Out is not an
// interactive terminal (piped or redirected) it returns the raw URL unchanged so
// scripts capture an exact value. Color is dropped when NO_COLOR is set.
func (r Renderer) HighlightURL(rawURL string) string {
	if !r.OutInteractive || !safeHTTPURL(rawURL) {
		return rawURL
	}
	label := rawURL
	if colorEnabled() {
		label = ansiBold + ansiUnderline + ansiCyan + rawURL + ansiReset
	}
	return "\x1b]8;;" + rawURL + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

// colorEnabled honors the NO_COLOR convention (https://no-color.org): any
// non-empty value disables ANSI color styling.
func colorEnabled() bool {
	return os.Getenv("NO_COLOR") == ""
}

type Spinner struct {
	once    sync.Once
	mu      sync.RWMutex
	message string
	stop    chan struct{}
	done    chan struct{}
}

func (r Renderer) StartSpinner(message string) *Spinner {
	spinner := &Spinner{message: message}
	if r.Mode != Human {
		return spinner
	}
	if !r.Interactive {
		r.Progress(message + "...")
		return spinner
	}

	spinner.stop = make(chan struct{})
	spinner.done = make(chan struct{})
	go func() {
		defer close(spinner.done)
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		index := 0
		for {
			_, _ = fmt.Fprintf(r.Err, "\r\x1b[2K%s %s", frames[index], spinner.Message())
			select {
			case <-spinner.stop:
				_, _ = fmt.Fprint(r.Err, "\r\x1b[2K")
				return
			case <-ticker.C:
				index = (index + 1) % len(frames)
			}
		}
	}()
	return spinner
}

// Update changes the text beside an interactive spinner without adding a new
// terminal line. Non-interactive and machine-readable renderers stay quiet.
func (s *Spinner) Update(message string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.message = message
	s.mu.Unlock()
}

func (s *Spinner) Message() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.message
}

func (s *Spinner) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.stop == nil {
			return
		}
		close(s.stop)
		<-s.done
	})
}

func safeHTTPURL(rawURL string) bool {
	if strings.ContainsAny(rawURL, "\x00\x07\x1b\r\n") {
		return false
	}
	parsed, err := url.Parse(rawURL)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}
