package command

import (
	"fmt"
	"io"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/output"
)

// renderTopic writes one help topic. It draws boxed panels when the result
// stream is an interactive terminal wide enough to hold them, and otherwise
// writes the plain indented form.
//
// Plain is not a degraded mode: it is what `beam --help | grep` sees, what a
// narrow terminal gets, and what --json embeds. Both renderings come from the
// same model so they cannot drift.
func renderTopic(w io.Writer, topic helpTopic, renderer output.Renderer) error {
	width := 0
	if renderer.OutInteractive {
		width = output.TerminalWidth(w)
	}
	if width == 0 {
		_, err := fmt.Fprint(w, plainTopic(topic))
		return err
	}
	return styledTopic(w, topic, renderer, width)
}

func styledTopic(w io.Writer, topic helpTopic, renderer output.Renderer, width int) error {
	if topic.Banner {
		if _, err := fmt.Fprintln(w, applyReleaseNames(banner)); err != nil {
			return err
		}
	}
	if topic.Summary != "" {
		if _, err := fmt.Fprintf(w, "%s\n\n", applyReleaseNames(topic.Summary)); err != nil {
			return err
		}
	}
	for _, usage := range topic.Usage {
		if _, err := fmt.Fprintf(w, "%s %s\n", renderer.Label("Usage:"), applyReleaseNames(usage)); err != nil {
			return err
		}
	}
	if len(topic.Usage) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	for _, section := range topic.Sections {
		panel := output.Panel{Title: section.Title}
		for _, entry := range section.Entries {
			panel.Entries = append(panel.Entries, output.PanelEntry{
				Name: applyReleaseNames(entry.Name),
				Desc: applyReleaseNames(entry.Desc),
			})
		}
		if err := renderer.Panel(w, panel, width); err != nil {
			return err
		}
	}
	for _, note := range topic.Notes {
		if _, err := fmt.Fprintf(w, "\n%s\n", wrapText(applyReleaseNames(note), width)); err != nil {
			return err
		}
	}
	if topic.Footer != "" {
		if _, err := fmt.Fprintf(w, "\n%s\n", applyReleaseNames(topic.Footer)); err != nil {
			return err
		}
	}
	return nil
}

// plainTopic is the canonical text form. renderHelp embeds exactly this string
// in --json, so it must never contain styling or box drawing.
func plainTopic(topic helpTopic) string {
	var builder strings.Builder
	if topic.Banner {
		builder.WriteString(banner)
		builder.WriteString("\n")
	}
	if topic.Summary != "" {
		builder.WriteString(topic.Summary)
		builder.WriteString("\n\n")
	}
	for index, usage := range topic.Usage {
		if index == 0 {
			builder.WriteString("Usage: " + usage + "\n")
			continue
		}
		builder.WriteString("       " + usage + "\n")
	}
	for _, section := range topic.Sections {
		builder.WriteString("\n" + section.Title + ":\n")
		nameWidth := 0
		for _, entry := range section.Entries {
			if entry.Desc != "" && len(entry.Name) > nameWidth {
				nameWidth = len(entry.Name)
			}
		}
		for _, entry := range section.Entries {
			if entry.Desc == "" {
				builder.WriteString("  " + entry.Name + "\n")
				continue
			}
			builder.WriteString("  " + entry.Name + strings.Repeat(" ", nameWidth-len(entry.Name)) + "  " + entry.Desc + "\n")
		}
	}
	for _, note := range topic.Notes {
		builder.WriteString("\n" + wrapText(note, 78) + "\n")
	}
	if topic.Footer != "" {
		builder.WriteString("\n" + topic.Footer + "\n")
	}
	return applyReleaseNames(builder.String())
}

// wrapText breaks a paragraph at the given width on word boundaries. Help prose
// is stored as single logical paragraphs so the width can follow the terminal
// rather than being frozen at authoring time.
func wrapText(text string, width int) string {
	if width < 20 {
		width = 20
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var builder strings.Builder
	lineLength := 0
	for index, word := range words {
		switch {
		case index == 0:
			builder.WriteString(word)
			lineLength = len(word)
		case lineLength+1+len(word) > width:
			builder.WriteString("\n" + word)
			lineLength = len(word)
		default:
			builder.WriteString(" " + word)
			lineLength += 1 + len(word)
		}
	}
	return builder.String()
}
