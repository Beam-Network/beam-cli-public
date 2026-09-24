package command

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestHelpShowsAuthActionRegistryRoomAndTunnel(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, expected := range []string{"auth", "action", "registry", "room", "tunnel"} {
		if !bytes.Contains(stdout.Bytes(), []byte(expected)) {
			t.Fatalf("help missing %q:\n%s", expected, stdout.String())
		}
	}
	if !bytes.Contains(stdout.Bytes(), []byte("|____/|_____/_/")) {
		t.Fatalf("help missing Beam ASCII banner:\n%s", stdout.String())
	}
}

func TestAuthWhoamiUsesCentralCredentialStoreWithoutPrintingToken(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", configDir)
	server := oauthContextServer(t)
	defer server.Close()
	t.Setenv("BEAM_AUTH_URL", server.URL)
	t.Setenv("BEAM_API_URL", server.URL)
	store := auth.Store{Path: filepath.Join(configDir, "credentials.json")}
	credentials := auth.Credentials{RefreshToken: "must-never-be-printed"}
	if err := store.Save(credentials); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"auth", "whoami", "--json"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("must-never-be-printed")) {
		t.Fatalf("token leaked in output: %s", stdout.String())
	}
	var result struct {
		Authenticated bool      `json:"authenticated"`
		User          auth.User `json:"user"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Authenticated || result.User.Email != "beam@example.com" {
		t.Fatalf("result=%#v", result)
	}
	if bytes.Contains(stdout.Bytes(), []byte("audiences")) ||
		bytes.Contains(stdout.Bytes(), []byte("sessions")) {
		t.Fatalf("service session details leaked into central identity output: %s", stdout.String())
	}
}

func TestAuthWhoamiHumanOutputDoesNotExposeServiceSessions(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", configDir)
	server := oauthContextServer(t)
	defer server.Close()
	t.Setenv("BEAM_AUTH_URL", server.URL)
	t.Setenv("BEAM_API_URL", server.URL)
	store := auth.Store{Path: filepath.Join(configDir, "credentials.json")}
	if err := store.Save(auth.Credentials{RefreshToken: "must-never-be-printed"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"auth", "whoami"},
		&stdout,
		&stderr,
	)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if got, want := stdout.String(), "beam@example.com\n"; got != want {
		t.Fatalf("stdout=%q want=%q", got, want)
	}
}

func TestAuthLogoutRemovesCentralCredentialStore(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", configDir)
	path := filepath.Join(configDir, "credentials.json")
	store := auth.Store{Path: path}
	if err := store.Save(auth.Credentials{RefreshToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/revoke" {
			t.Fatalf("unexpected path=%q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("token") != "secret" || r.Form.Get("client_id") != auth.ClientID {
			t.Fatalf("form=%v", r.Form)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("BEAM_AUTH_URL", server.URL)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"auth", "logout"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Active() {
		t.Fatalf("credentials still present: %#v", loaded)
	}
}

func TestAuthLoginOpensBrowserAndUsesSingleLineSpinner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/device/authorize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"verification_uri":          "https://auth.example/device",
				"verification_uri_complete": "https://auth.example/device?user_code=BEAM-CODE",
				"user_code":                 "BEAM-CODE",
				"device_code":               "device-secret",
				"interval":                  1,
				"expires_in":                60,
			})
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "must-never-be-printed",
				"token_type":    "Bearer",
				"expires_in":    900,
				"refresh_token": "must-never-be-printed-refresh",
				"scope":         auth.Scope,
			})
		case "/api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accountType": "user",
				"user":        map[string]string{"id": "user-1", "email": "beam@example.com"},
			})
		case "/api/organizations":
			_ = json.NewEncoder(w).Encode(map[string]any{"organizations": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AUTH_URL", server.URL)
	t.Setenv("BEAM_API_URL", server.URL)
	app := New(version.BuildInfo{Version: "test"})
	app.authStore = func(path string) auth.Store { return auth.Store{Path: path} }
	app.isTerminal = func(io.Writer) bool { return true }
	var openedURL string
	app.openURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"auth", "login"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if openedURL != "https://auth.example/device?user_code=BEAM-CODE" {
		t.Fatalf("opened URL=%q", openedURL)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("\x1b]8;;"+openedURL)) {
		t.Fatalf("verification URL is not clickable: %q", stderr.String())
	}
	if bytes.Count(stderr.Bytes(), []byte("Waiting for authorization...\n")) != 0 {
		t.Fatalf("polling was logged as lines: %q", stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("\r\x1b[2K")) {
		t.Fatalf("spinner did not update in place: %q", stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("must-never-be-printed")) ||
		bytes.Contains(stderr.Bytes(), []byte("must-never-be-printed")) {
		t.Fatal("token leaked in login output")
	}
}

func oauthContextServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "refreshed-access", "token_type": "Bearer", "expires_in": 900,
				"refresh_token": "rotated-refresh", "scope": auth.Scope,
			})
		case "/api/me":
			if r.Header.Get("Authorization") != "Bearer refreshed-access" {
				t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accountType": "user",
				"user":        map[string]string{"id": "user-1", "email": "beam@example.com"},
			})
		case "/api/organizations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"organizations": []map[string]any{{"id": "org-1", "name": "Beam", "role": "OWNER"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestRegistryAuthCommandsPointToCentralAuth(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"registry", "login"},
		&stdout,
		&stderr,
	)
	if code != ExitUsage {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte(version.CLIName()+" auth login")) {
		t.Fatalf("missing central auth guidance: %s", stderr.String())
	}
}

func TestRegistryVersionsAgainstFakeHTTPServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/packages/@beam/example/versions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{{
				"packageName": "@beam/example",
				"version":     "1.0.0",
				"status":      "active",
			}},
		})
	}))
	defer server.Close()
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_REGISTRY_URL", server.URL)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"--json", "registry", "versions", "@beam/example"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
}

func TestJSONUsageErrorHasNoStdoutNoise(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"registry", "pack", "--json"},
		&stdout,
		&stderr,
	)
	if code != ExitUsage {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q", stdout.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatalf("invalid error JSON: %v\n%s", err, stderr.String())
	}
}

func TestUnknownOptionIsRejected(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"registry", "inspect", "--wat"},
		&stdout,
		&stderr,
	)
	if code != ExitUsage {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestDaemonUnavailableExitCode(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", "/definitely/missing/beam-agent.sock")
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"tunnel", "status"},
		&stdout,
		&stderr,
	)
	if code != ExitDaemonUnavailable {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if bytes.Contains(stderr.Bytes(), []byte("tunnel start")) || bytes.Contains(stderr.Bytes(), []byte("agent start")) {
		t.Fatalf("error still requests a manual daemon start: %s", stderr.String())
	}
}
