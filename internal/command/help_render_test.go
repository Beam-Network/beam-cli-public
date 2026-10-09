package command

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func allTopics(t *testing.T) map[string]helpTopic {
	t.Helper()
	topics := map[string]helpTopic{"": rootHelpTopic}
	for name, topic := range helpTopics {
		topics[name] = topic
	}
	for name, topic := range commandTopics {
		topics[name] = topic
	}
	return topics
}

// The plain rendering is what --json embeds and what a pipe receives, so it must
// never carry styling or box drawing.
func TestPlainTopicsCarryNoStylingOrBoxDrawing(t *testing.T) {
	for name, topic := range allTopics(t) {
		text := plainTopic(topic)
		if strings.Contains(text, "\x1b") {
			t.Errorf("topic %q plain text contains escape sequences", name)
		}
		if strings.ContainsAny(text, "╭╮╰╯│") {
			t.Errorf("topic %q plain text contains box drawing", name)
		}
		if strings.TrimSpace(text) == "" {
			t.Errorf("topic %q renders empty", name)
		}
	}
}

// Styled and plain come from one model, so every entry name in a topic has to
// survive both renderings. This is what stops the boxed form from quietly
// dropping content.
func TestStyledAndPlainCarryTheSameEntries(t *testing.T) {
	renderer := output.Renderer{Mode: output.Human, OutInteractive: true}
	for name, topic := range allTopics(t) {
		var styled bytes.Buffer
		if err := styledTopic(&styled, topic, renderer, 92); err != nil {
			t.Fatalf("topic %q: %v", name, err)
		}
		plain := plainTopic(topic)
		for _, section := range topic.Sections {
			for _, entry := range section.Entries {
				want := applyReleaseNames(entry.Name)
				if !strings.Contains(plain, want) {
					t.Errorf("topic %q plain text is missing entry %q", name, want)
				}
				// A very long name can be truncated inside a panel, so compare on a
				// prefix that fits any box.
				probe := want
				if len(probe) > 20 {
					probe = probe[:20]
				}
				if !strings.Contains(styled.String(), probe) {
					t.Errorf("topic %q styled output is missing entry %q", name, want)
				}
			}
			if !strings.Contains(plain, section.Title) || !strings.Contains(styled.String(), section.Title) {
				t.Errorf("topic %q is missing section %q in one rendering", name, section.Title)
			}
		}
	}
}

// The root help is the CLI's table of contents. A command that is dispatchable
// but absent here is invisible, so the grouping is pinned.
func TestRootHelpListsEveryCommand(t *testing.T) {
	want := []string{
		"action", "agent", "auth", "budget", "completion", "config", "context", "doctor",
		"help",
		"login", "logout", "logs", "org", "registry", "room",
		"session", "setup", "status", "studio",
		"update", "version", "whoami",
	}
	var got []string
	for _, section := range rootHelpTopic.Sections {
		if section.Title == "Global options" {
			continue
		}
		for _, entry := range section.Entries {
			got = append(got, entry.Name)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("root commands =\n%v\nwant\n%v", got, want)
	}
}

func TestEveryRootCommandHasHelp(t *testing.T) {
	for _, section := range rootHelpTopic.Sections {
		if section.Title == "Global options" {
			continue
		}
		for _, entry := range section.Entries {
			if _, ok := helpTopicPath([]string{entry.Name}); !ok {
				t.Errorf("command %q has no help topic", entry.Name)
			}
		}
	}
}

// A usage line is stored without its "Usage:" label so the renderer can style
// the label, which leaves the CLI name at position zero where the prefix rules
// could not reach it. A beam-dev build printed "beam" before this was handled.
func TestApplyReleaseNamesRewritesABareUsageLine(t *testing.T) {
	got := applyReleaseNames("beam room list [--state <active|closed|all>]")
	if !strings.HasPrefix(got, cliNameForTest()) {
		t.Errorf("usage line = %q, want it to start with %q", got, cliNameForTest())
	}
	if strings.HasPrefix(got, "beam ") && cliNameForTest() != "beam" {
		t.Errorf("usage line kept the default name: %q", got)
	}
}

func cliNameForTest() string {
	return strings.Fields(applyReleaseNames("beam x"))[0]
}

// Rendering falls back to plain text when the terminal is too narrow for a box,
// rather than drawing one that wraps.
func TestRenderTopicFallsBackToPlainWithoutATerminal(t *testing.T) {
	var out bytes.Buffer
	renderer := output.Renderer{Mode: output.Human, Out: &out, OutInteractive: true}
	if err := renderTopic(&out, rootHelpTopic, renderer); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "╭╮╰╯│") {
		t.Error("non-terminal writer received box drawing")
	}
	if !strings.Contains(out.String(), "Getting started:") {
		t.Error("plain fallback lost its section headings")
	}
}

// "beam <command> --help" works for every command; help was the exception,
// because the help branch runs before the flag is interpreted.
func TestHelpFlagIsATopicRequestNotATopicName(t *testing.T) {
	enableTunnels(t)
	run := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr)
		if code != ExitOK {
			return stderr.String(), code
		}
		return stdout.String(), code
	}

	helpHelp, code := run(t, "help", "help")
	if code != ExitOK {
		t.Fatalf("beam help help code=%d out=%s", code, helpHelp)
	}
	root, code := run(t, "help")
	if code != ExitOK {
		t.Fatalf("beam help code=%d out=%s", code, root)
	}
	tunnelHelp, code := run(t, "help", "tunnel")
	if code != ExitOK {
		t.Fatalf("beam help tunnel code=%d out=%s", code, tunnelHelp)
	}

	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{"--help matches help help", []string{"help", "--help"}, helpHelp},
		{"-h matches help help", []string{"help", "-h"}, helpHelp},
		{"bare help still renders the root", []string{"help"}, root},
		{"a flagged topic still renders that topic", []string{"help", "tunnel", "--help"}, tunnelHelp},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, code := run(t, testCase.args...)
			if code != ExitOK {
				t.Fatalf("code=%d out=%s", code, got)
			}
			if got != testCase.want {
				t.Fatalf("beam %s rendered:\n%s\nwant:\n%s", strings.Join(testCase.args, " "), got, testCase.want)
			}
		})
	}
}

func TestRoomMediaHelpListsPublisherAndViewerFlags(t *testing.T) {
	for _, args := range [][]string{
		{"help", "room"},
		{"room", "btr_room_a", "channel", "btr_channel_a", "media", "publish", "--help"},
		{"room", "btr_room_a", "channel", "btr_channel_a", "media", "view", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			var stdout, stderr bytes.Buffer
			if code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			for _, flag := range []string{"--as", "--session", "--all", "--sdp", "--open", "--snippets"} {
				if !strings.Contains(stdout.String(), flag) {
					t.Errorf("help is missing %s:\n%s", flag, stdout.String())
				}
			}
		})
	}
}
