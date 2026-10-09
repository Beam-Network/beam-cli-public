package command

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func enableTunnels(t *testing.T) {
	t.Helper()
	t.Setenv(experimentalTunnelsEnv, "1")
}

func TestTunnelCommandsAreUnavailableByDefault(t *testing.T) {
	t.Setenv(experimentalTunnelsEnv, "")
	// No daemon listens here: a command that reached it would fail with a
	// daemon error instead of the tunnels-unavailable message.
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	for _, args := range [][]string{
		{"share", "8080"},
		{"receive", "."},
		{"unshare", "--last"},
		{"transfer", "list"},
		{"operation", "list"},
		{"tunnel"},
		{"tunnel", "expose", "http", "8080"},
		{"tunnel", "list"},
		{"tunnel", "start"},
		{"tunnel", "expose", "--help"},
		{"help", "tunnel"},
		{"help", "share"},
	} {
		var stdout, stderr bytes.Buffer
		code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr)
		if code != ExitUsage || !strings.Contains(stderr.String(), "Tunnels are not available yet.") {
			t.Errorf("%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestRoomAndStudioAliasesAreNotTunnelCommands(t *testing.T) {
	for _, args := range [][]string{{"room", "list"}, {"studio", "status"}, {"tunnel", "room", "list"}, {"tunnel", "studio", "status"}} {
		if isTunnelCommand(args) {
			t.Errorf("%v is treated as a tunnel command", args)
		}
	}
}

func TestHelpAndCompletionDoNotAdvertiseTunnels(t *testing.T) {
	enableTunnels(t)
	for _, section := range rootHelpTopic.Sections {
		for _, entry := range section.Entries {
			if isTunnelCommand([]string{entry.Name}) {
				t.Errorf("root help lists %q", entry.Name)
			}
		}
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		script, _ := rawCompletionScript(shell)
		for _, word := range []string{"tunnel", "share", "receive", "transfer", "operation"} {
			if strings.Contains(script, word) {
				t.Errorf("%s completion mentions %q", shell, word)
			}
		}
	}
}
