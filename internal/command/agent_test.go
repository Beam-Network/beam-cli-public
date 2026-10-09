//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// Status only reports: after `agent stop`, `agent status` must show the agent
// stopped instead of starting it again.
func TestAgentStatusNeverStartsTheDaemon(t *testing.T) {
	// Nothing listens on the socket: the daemon is stopped.
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", t.TempDir()+"/stopped.sock")
	app := New(version.BuildInfo{Version: "test"})
	var starts atomic.Int32
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		starts.Add(1)
		return tunnel.StartResult{}, errors.New("status must not start the daemon")
	}
	for args, want := range map[string]string{
		"agent status":        "is stopped. Run `beam agent start`",
		"agent status --json": `{"state":"stopped","connected":false,"daemon_running":false}`,
	} {
		var stdout, stderr bytes.Buffer
		if code := app.Run(context.Background(), strings.Fields(args), &stdout, &stderr); code != ExitOK {
			t.Fatalf("%s: code=%d stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("%s: stdout=%q, want %q", args, stdout.String(), want)
		}
	}
	if starts.Load() != 0 {
		t.Fatalf("daemon starts=%d, want 0", starts.Load())
	}
}

func TestAgentStatusAgainstFakeDaemon(t *testing.T) {
	status := tunnel.AgentConnectionStatus{Prepared: true, Registered: true, Connected: true, State: "connected",
		AgentID: "agt_test", Label: "builder", CoordinatorURL: "https://coordinator.example.com"}
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case tunnel.AgentConnectionPath:
			_ = json.NewEncoder(w).Encode(status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	app := New(version.BuildInfo{Version: "test"})
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"agent", "status", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var got tunnel.AgentConnectionStatus
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AgentID != status.AgentID || !got.Registered {
		t.Fatalf("status = %+v", got)
	}
}

func TestAgentDisconnectStopsDaemonAfterClearingIdentity(t *testing.T) {
	// See the doctor onboarding regression immediately below.
	var mu sync.Mutex
	var calls []string
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case r.Method == http.MethodDelete && r.URL.Path == tunnel.AgentConnectionPath:
			_ = json.NewEncoder(w).Encode(tunnel.AgentConnectionStatus{State: "prepared", Prepared: true})
		case r.Method == http.MethodPost && r.URL.Path == tunnel.ShutdownPath:
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(tunnel.ShutdownResponse{Stopping: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	app := New(version.BuildInfo{Version: "test"})
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"agent", "disconnect"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{http.MethodGet + " " + tunnel.StatusPath, http.MethodDelete + " " + tunnel.AgentConnectionPath, http.MethodPost + " " + tunnel.ShutdownPath}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls=%#v want=%#v", calls, want)
	}
}

func TestDoctorAuthenticationMatchesOnboardingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                                                       string
		roomOnly, registered, connected, account, corrupt, healthy bool
	}{
		{"invited member", true, true, true, false, false, true},
		{"disconnected invited member", true, true, false, false, false, false},
		{"unregistered invited member", true, false, true, false, false, false},
		{"account requires login", false, true, true, false, false, false},
		{"account session", false, true, true, true, false, true},
		{"corrupt credentials", true, true, true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tunnel.StatusPath:
					_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
				case tunnel.AgentConnectionPath:
					_ = json.NewEncoder(w).Encode(tunnel.AgentConnectionStatus{RoomJoinOnly: tc.roomOnly, Registered: tc.registered, Connected: tc.connected, State: "connected"})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			app := New(version.BuildInfo{Version: "test"})
			app.authStore = func(path string) auth.Store {
				store := auth.Store{Path: path}
				if tc.account {
					if err := store.Save(auth.Credentials{RefreshToken: "test-account-session"}); err != nil {
						t.Fatal(err)
					}
				}
				if tc.corrupt {
					if err := os.WriteFile(path, []byte("invalid credential JSON"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return store
			}
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), []string{"doctor", "--json"}, &stdout, &stderr); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			var result struct {
				Healthy bool `json:"healthy"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Healthy != tc.healthy {
				t.Fatalf("healthy=%v want=%v output=%s", result.Healthy, tc.healthy, stdout.String())
			}
		})
	}
}

func TestSelectAgentOrganization(t *testing.T) {
	organizations := []auth.Organization{{ID: "org_a", Slug: "a"}, {ID: "org_b", Slug: "b", IsDefault: true}}
	selected, err := selectAgentOrganization(organizations, "")
	if err != nil || selected.ID != "org_b" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	selected, err = selectAgentOrganization(organizations, "a")
	if err != nil || selected.ID != "org_a" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

func TestSelectAgentOrganizationEdgeCases(t *testing.T) {
	tests := []struct {
		name          string
		organizations []auth.Organization
		requested     string
		wantID        string
		wantKind      string
	}{
		{name: "none", wantKind: errorKindOrganizationRequired},
		{name: "restricted", organizations: []auth.Organization{{ID: "blocked", RestrictionStatus: "BLOCKED"}}, wantKind: errorKindOrganizationUnavailable},
		{name: "one accessible among restricted", organizations: []auth.Organization{{ID: "limited", RestrictionStatus: "LIMITED"}, {ID: "active"}}, wantID: "active"},
		{name: "multiple without default", organizations: []auth.Organization{{ID: "one"}, {ID: "two"}}, wantKind: errorKindOrganizationSelection},
		{name: "stale explicit selection", organizations: []auth.Organization{{ID: "current"}}, requested: "deleted", wantKind: errorKindOrganizationNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			organization, err := selectAgentOrganization(test.organizations, test.requested)
			if organization.ID != test.wantID {
				t.Fatalf("organization=%+v want=%q", organization, test.wantID)
			}
			if test.wantKind == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var cliErr *Error
			if !errors.As(err, &cliErr) {
				t.Fatalf("err=%v is not a CLI error", err)
			}
			if cliErr.Kind != test.wantKind {
				t.Fatalf("err=%v kind=%q want=%q", err, cliErr.Kind, test.wantKind)
			}
		})
	}
}

func TestRegisteredAgentRejectsDestructiveSetupChanges(t *testing.T) {
	status := tunnel.AgentConnectionStatus{
		Registered: true, CoordinatorURL: "https://coordinator.example.com", OrganizationID: "org-a",
	}
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "coordinator", err: validateRegisteredAgentSelection(status, "https://other.example.com", true, "", false, nil)},
		{name: "organization", err: validateRegisteredAgentSelection(status, status.CoordinatorURL, false, "org-b", true, nil)},
		{name: "room identity", err: validateRegisteredAgentSelection(tunnel.AgentConnectionStatus{Registered: true, RoomJoinOnly: true}, "", false, "", false, nil)},
		{name: "revoked", err: validateRegisteredAgentSelection(tunnel.AgentConnectionStatus{Registered: true, State: "revoked"}, "", false, "", false, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cliErr *Error
			if !errors.As(test.err, &cliErr) || (cliErr.Kind != errorKindAgentConflict && cliErr.Kind != errorKindAgentRevoked) {
				t.Fatalf("err=%v", test.err)
			}
		})
	}
	if err := validateRegisteredAgentSelection(status, status.CoordinatorURL+"/", true, "org-a", true, nil); err != nil {
		t.Fatalf("same selection rejected: %v", err)
	}
}

func TestAgentLabelValidation(t *testing.T) {
	if label, err := validAgentLabel("  workstation  "); err != nil || label != "workstation" {
		t.Fatalf("label=%q err=%v", label, err)
	}
	if _, err := validAgentLabel("bad\nlabel"); err == nil {
		t.Fatal("control character was accepted")
	}
}

func TestSetupWithoutOrganizationsKeepsLoginAndCanFinishLater(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access", "refresh_token": "rotated", "token_type": "Bearer", "expires_in": 900, "scope": auth.Scope,
			})
		case "/api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"accountType": "user", "user": map[string]string{"id": "user-a", "email": "user@example.com"}})
		case "/api/organizations":
			_ = json.NewEncoder(w).Encode(map[string]any{"organizations": []any{}})
		default:
			http.NotFound(w, request)
		}
	}))
	defer apiServer.Close()
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == tunnel.AgentConnectionPath:
			_ = json.NewEncoder(w).Encode(tunnel.AgentConnectionStatus{Prepared: true, State: "prepared"})
		case request.Method == http.MethodPost && request.URL.Path == tunnel.AgentConnectionPath+"/prepare":
			_ = json.NewEncoder(w).Encode(tunnel.AgentConnectionStatus{Prepared: true, State: "prepared", PublicKeyFingerprint: "sha256:test"})
		default:
			http.NotFound(w, request)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	t.Setenv("BEAM_AUTH_URL", apiServer.URL)
	t.Setenv("BEAM_API_URL", apiServer.URL)
	t.Setenv("BEAM_COORDINATOR_URL", "https://coordinator.example.com")
	cfg, paths, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := auth.Store{Path: paths.Credentials}
	if err := store.Save(auth.Credentials{RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	app := New(version.BuildInfo{Version: "test"})
	app.authStore = func(string) auth.Store { return store }
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		return tunnel.StartResult{AlreadyRunning: true}, nil
	}
	app.openURL = func(string) error { return nil }
	responses := []string{"3", "n"}
	app.readLine = func() (string, error) {
		response := responses[0]
		responses = responses[1:]
		return response, nil
	}
	var stdout, stderr bytes.Buffer
	err = app.setup(context.Background(), []string{"--mode", "account"}, cfg, paths, output.Renderer{
		Mode: output.Human, Out: &stdout, Err: &stderr, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.Load()
	if err != nil || !credentials.Active() {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("Run `beam setup`")) || !bytes.Contains(stderr.Bytes(), []byte("No Beam organization")) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
