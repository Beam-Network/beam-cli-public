package command

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestInteractiveSetupCanBeDeferred(t *testing.T) {
	app := New(version.BuildInfo{Version: "test"})
	responses := []string{"invalid", "3", "n"}
	app.readLine = func() (string, error) {
		response := responses[0]
		responses = responses[1:]
		return response, nil
	}
	var stdout, stderr bytes.Buffer
	err := app.setup(context.Background(), nil, config.Config{}, config.Paths{}, output.Renderer{
		Mode: output.Human, Out: &stdout, Err: &stderr, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Run `beam setup`") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Please enter 1, 2, or 3") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestApplyReleaseNamesRewritesStandaloneCommand(t *testing.T) {
	if got, want := applyReleaseNames("beam status"), version.CLIName()+" status"; got != want {
		t.Fatalf("applyReleaseNames() = %q, want %q", got, want)
	}
}

func TestOrganizationPromptAcceptsNumberAfterInvalidChoice(t *testing.T) {
	app := New(version.BuildInfo{Version: "test"})
	responses := []string{"missing", "2"}
	app.readLine = func() (string, error) {
		response := responses[0]
		responses = responses[1:]
		return response, nil
	}
	organizations := []auth.Organization{
		{ID: "org_a", Slug: "alpha", Name: "Alpha"},
		{ID: "org_b", Slug: "beta", Name: "Beta"},
	}
	var stderr bytes.Buffer
	selected, err := app.promptOrganization(organizations, output.Renderer{Mode: output.Human, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "org_b" {
		t.Fatalf("selected=%+v", selected)
	}
	if !strings.Contains(stderr.String(), "Alpha (alpha)") || !strings.Contains(stderr.String(), "Beta (beta)") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRequiredPromptRepeatsAfterEmptyInput(t *testing.T) {
	app := New(version.BuildInfo{Version: "test"})
	responses := []string{"", "room-a"}
	app.readLine = func() (string, error) {
		response := responses[0]
		responses = responses[1:]
		return response, nil
	}
	var stderr bytes.Buffer
	value, err := app.promptText(output.Renderer{Mode: output.Human, Err: &stderr}, "Room ID", "", true)
	if err != nil || value != "room-a" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if !strings.Contains(stderr.String(), "Room ID is required") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestPowerShellCompletionInstallIsIdempotentAndPreservesProfile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "Microsoft.PowerShell_profile.ps1")
	if err := os.WriteFile(profile, []byte("# existing profile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEAM_POWERSHELL_PROFILE", profile)
	for range 2 {
		path, shell, err := installCompletion("powershell")
		if err != nil {
			t.Fatal(err)
		}
		if path != profile || shell != "powershell" {
			t.Fatalf("path=%q shell=%q", path, shell)
		}
	}
	installed, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := powerShellCompletionMarkers()
	if strings.Count(string(installed), start) != 1 || !strings.Contains(string(installed), "Register-ArgumentCompleter") {
		t.Fatalf("profile=%q", installed)
	}
	changed, err := uninstallPowerShellCompletion(profile)
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	remaining, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(remaining) != "# existing profile\n" {
		t.Fatalf("remaining=%q", remaining)
	}
}

func TestAgentSetupArgsExcludeOnboardingOnlyOptions(t *testing.T) {
	args := agentSetupArgs([]string{
		"--mode", "account", "--coordinator", "https://coordinator.example.com",
		"--organization", "beam", "--label", "laptop", "--completion", "zsh",
	})
	want := "--coordinator https://coordinator.example.com --organization beam --label laptop"
	if strings.Join(args, " ") != want {
		t.Fatalf("args=%q want=%q", strings.Join(args, " "), want)
	}
}

func TestNonInteractiveSetupRequiresExplicitMode(t *testing.T) {
	err := New(version.BuildInfo{Version: "test"}).setup(context.Background(), nil, config.Config{}, config.Paths{}, output.Renderer{
		Mode: output.JSON, Out: &bytes.Buffer{}, Err: &bytes.Buffer{},
	})
	var cliErr *Error
	if !errors.As(err, &cliErr) || cliErr.Kind != errorKindSetupModeRequired {
		t.Fatalf("err=%v", err)
	}
}

func TestSetupRejectsOptionsFromAnotherMode(t *testing.T) {
	for _, test := range []struct {
		mode string
		args []string
	}{
		{mode: setupModeAccount, args: []string{"--mode", setupModeAccount, "--room", "room-a"}},
		{mode: setupModeRoom, args: []string{"--mode", setupModeRoom, "--organization", "org-a"}},
		{mode: setupModeSkip, args: []string{"--mode", setupModeSkip, "--coordinator", "https://coordinator.example.com"}},
	} {
		if err := validateSetupModeOptions(test.args, test.mode); err == nil {
			t.Fatalf("mode=%s args=%v were accepted", test.mode, test.args)
		}
	}
}

func TestSetupModeRequirementIsMachineReadable(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"setup", "--json"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"kind":"setup_mode_required"`) {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestOrganizationPromptSkipsRestrictedEntries(t *testing.T) {
	app := New(version.BuildInfo{Version: "test"})
	app.readLine = func() (string, error) { return "1", nil }
	organizations := []auth.Organization{
		{ID: "blocked", Name: "Blocked", RestrictionStatus: "BLOCKED"},
		{ID: "active", Name: "Active"},
	}
	var stderr bytes.Buffer
	selected, err := app.promptOrganization(organizations, output.Renderer{Mode: output.Human, Err: &stderr})
	if err != nil || selected.ID != "active" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	if !strings.Contains(stderr.String(), "unavailable: BLOCKED") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRoomInvitationErrorsHaveStableKinds(t *testing.T) {
	for _, test := range []struct {
		code string
		kind string
	}{
		{code: "invitation_expired", kind: errorKindInvitationExpired},
		{code: "invitation_revoked", kind: errorKindInvitationRevoked},
		{code: "invitation_consumed", kind: errorKindInvitationConsumed},
		{code: "invitation_room_mismatch", kind: errorKindInvitationRoomMismatch},
	} {
		err := mapRoomInvitationError(&tunnel.Error{Kind: tunnel.ErrConflict, Code: test.code})
		var cliErr *Error
		if !errors.As(err, &cliErr) || cliErr.Kind != test.kind {
			t.Fatalf("code=%q err=%v", test.code, err)
		}
	}
}

func TestRoomJoinIdempotencyKeyIsStableAndScoped(t *testing.T) {
	first := roomJoinIdempotencyKey("room-a", "secret")
	if first != roomJoinIdempotencyKey("room-a", "secret") {
		t.Fatal("idempotency key is not stable")
	}
	if first == roomJoinIdempotencyKey("room-b", "secret") || strings.Contains(first, "secret") {
		t.Fatalf("unsafe or unscoped key %q", first)
	}
}

func TestOrganizationOnboardingURL(t *testing.T) {
	if got := organizationOnboardingURL("https://api.b1m.ai"); got != "https://console.b1m.ai/onboarding" {
		t.Fatalf("production URL=%q", got)
	}
	if got := organizationOnboardingURL("https://api.other.example.test"); got != "" {
		t.Fatalf("unknown host URL=%q", got)
	}
	originalAPIURL := config.DefaultAPIURL
	t.Cleanup(func() { config.DefaultAPIURL = originalAPIURL })
	config.DefaultAPIURL = "https://api.other.example.test"
	if got := organizationOnboardingURL("https://api.other.example.test"); got != "https://console.other.example.test/onboarding" {
		t.Fatalf("compiled-in environment URL=%q", got)
	}
	t.Setenv("BEAM_CONSOLE_URL", "https://private.example/console")
	if got := organizationOnboardingURL("https://api.b1m.ai"); got != "https://private.example/console/onboarding" {
		t.Fatalf("override URL=%q", got)
	}
}
