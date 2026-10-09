package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestHelpAndVersionWorkWithInvalidConfiguration(t *testing.T) {
	t.Setenv("BEAM_OUTPUT", "invalid")
	for _, args := range [][]string{{"help"}, {"--version"}, {"registry", "publish", "--help"}} {
		var stdout, stderr bytes.Buffer
		code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr)
		if code != ExitOK {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
	}
}

func TestRoomUnavailableUsesGlobalJSONContract(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", directory)
	t.Setenv("BEAM_AGENT_SOCKET", filepath.Join(directory, "missing.sock"))
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"room", "list", "--json"}, &stdout, &stderr)
	if code != ExitDaemonUnavailable {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var result struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &result); err != nil || result.Error.Code != ExitDaemonUnavailable {
		t.Fatalf("stderr=%q err=%v", stderr.String(), err)
	}
}

func TestActionInitAndDefaultPack(t *testing.T) {
	configDirectory := t.TempDir()
	actionDirectory := filepath.Join(t.TempDir(), "example")
	t.Setenv("BEAM_CONFIG_DIR", configDirectory)
	app := New(version.BuildInfo{Version: "test"})
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"action", "init", actionDirectory, "--name", "@test/example"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("init code=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run(context.Background(), []string{"action", "pack", actionDirectory}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("pack code=%d stderr=%s", code, stderr.String())
	}
	matches, err := filepath.Glob(filepath.Join(actionDirectory, "dist", "*.tgz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("archives=%v err=%v", matches, err)
	}
}

func TestScopedRoomChannelGrammar(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"events", "publish", "--literal", "ok"}, []string{"channel", "publish", "room", "events", "--literal", "ok"}},
		{[]string{"events", "stream", "listen"}, []string{"channel", "stream", "room", "listen", "events"}},
		{[]string{"files", "object", "watch", "publication"}, []string{"channel", "object", "status", "room", "files", "publication", "--follow"}},
		{[]string{"events", "grant", "list"}, []string{"grant", "list", "room", "events"}},
	}
	for _, test := range tests {
		got, err := normalizeScopedChannel("room", test.args)
		if err != nil || strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
			t.Fatalf("args=%v got=%v want=%v err=%v", test.args, got, test.want, err)
		}
	}
}

func TestNamedTunnelStateIsContextScoped(t *testing.T) {
	paths := config.Paths{Dir: t.TempDir(), ContextName: "dev"}
	if err := rememberTunnel(paths, "api", "endpoint-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveTunnel(paths, "api"); err != nil || got != "endpoint-1" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if got, err := resolveTunnel(paths, "--last"); err != nil || got != "endpoint-1" {
		t.Fatalf("last=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(paths.Dir, "resources-dev.json")); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalOptionsAndBooleanTunnelOptions(t *testing.T) {
	args, globals, err := globalArgs([]string{"share", "3000", "--context=dev", "--no-interactive", "--verbose"})
	if err != nil || strings.Join(args, " ") != "share 3000" || globals.Context != "dev" || !globals.NoInteractive || !globals.Verbose {
		t.Fatalf("args=%v globals=%+v err=%v", args, globals, err)
	}
	positionals, err := positionalsForOptions(
		[]string{"--discover-coordinators", "--transport", "quic"},
		map[string]bool{"--transport": true},
		map[string]bool{"--discover-coordinators": true},
	)
	if err != nil || len(positionals) != 0 {
		t.Fatalf("positionals=%v err=%v", positionals, err)
	}
}

func TestMissingSessionDoesNotStartDeviceFlowWhenNonInteractive(t *testing.T) {
	directory := t.TempDir()
	app := New(version.BuildInfo{Version: "test"})
	app.authStore = func(path string) auth.Store { return auth.Store{Path: path} }
	_, err := app.ensureAgentUserSession(context.Background(), config.Config{}, config.Paths{Credentials: filepath.Join(directory, "credentials.json")}, output.Renderer{})
	var cliErr *Error
	if err == nil || !errors.As(err, &cliErr) || cliErr.Code != ExitAuth {
		t.Fatalf("err=%v", err)
	}
}

// The stored user outlives the refresh token, so status has to report the
// session, not merely whoever last logged in.
func TestStatusReportsSessionStateNotStoredIdentity(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		refreshToken string
		wantAccount  string
	}{
		{"expired session keeps the stored user", "", "Account: not logged in"},
		{"active session names the account", "refresh", "Account: beam@example.com"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("BEAM_CONFIG_DIR", directory)
			t.Setenv("BEAM_AGENT_SOCKET", filepath.Join(directory, "missing.sock"))
			writeCredentials(t, directory, testCase.refreshToken)

			var stdout, stderr bytes.Buffer
			code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"status"}, &stdout, &stderr)
			if code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), testCase.wantAccount) {
				t.Fatalf("stdout=%q want %q", stdout.String(), testCase.wantAccount)
			}
		})
	}
}

// The JSON contract reports the session separately from the identity and must
// keep doing so in both states.
func TestStatusJSONSeparatesAuthenticationFromUser(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		refreshToken string
		want         bool
	}{
		{"expired", "", false},
		{"active", "refresh", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("BEAM_CONFIG_DIR", directory)
			t.Setenv("BEAM_AGENT_SOCKET", filepath.Join(directory, "missing.sock"))
			writeCredentials(t, directory, testCase.refreshToken)

			var stdout, stderr bytes.Buffer
			if code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"status", "--json"}, &stdout, &stderr); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			var result struct {
				Authenticated bool `json:"authenticated"`
				User          *struct {
					Email string `json:"email"`
				} `json:"user"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("stdout=%q err=%v", stdout.String(), err)
			}
			if result.Authenticated != testCase.want {
				t.Fatalf("authenticated=%v want=%v", result.Authenticated, testCase.want)
			}
			if result.User == nil || result.User.Email != "beam@example.com" {
				t.Fatalf("user=%+v, the stored identity must stay in the JSON contract", result.User)
			}
		})
	}
}

// writeCredentials stores a credentials file directly. Store.Save refuses to
// write one without a refresh token, but that is exactly the state left behind
// when a session expires: the user block outlives the token.
func writeCredentials(t *testing.T, directory, refreshToken string) {
	t.Helper()
	credentials := auth.Credentials{
		Version:      2,
		RefreshStore: "file",
		RefreshToken: refreshToken,
		User:         &auth.User{Email: "beam@example.com"},
	}
	data, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
