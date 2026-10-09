//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const testAPIKey = "b1m_test_secret_key_value"

// apiKeyEnrollmentFixture fakes beam-agentd, the Beam API key verification
// route, and the coordinator enrollment route, recording what each received.
type apiKeyEnrollmentFixture struct {
	app            *App
	verifyStatus   int
	verifyBody     map[string]any
	enrollStatus   int
	mu             sync.Mutex
	verifyCalls    int
	enrollHeaders  http.Header
	enrollBody     map[string]any
	redeemedTokens []string
}

func newAPIKeyEnrollmentFixture(t *testing.T) *apiKeyEnrollmentFixture {
	t.Helper()
	fixture := &apiKeyEnrollmentFixture{
		verifyStatus: http.StatusOK,
		verifyBody:   map[string]any{"valid": true, "keyId": "key_1", "organizationId": "org_key"},
		enrollStatus: http.StatusCreated,
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/keys/verify" {
			http.NotFound(w, r)
			return
		}
		fixture.mu.Lock()
		fixture.verifyCalls++
		fixture.mu.Unlock()
		w.WriteHeader(fixture.verifyStatus)
		_ = json.NewEncoder(w).Encode(fixture.verifyBody)
	}))
	t.Cleanup(api.Close)
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/agent-enrollments" {
			http.NotFound(w, r)
			return
		}
		fixture.mu.Lock()
		fixture.enrollHeaders = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&fixture.enrollBody)
		fixture.mu.Unlock()
		w.WriteHeader(fixture.enrollStatus)
		if fixture.enrollStatus == http.StatusCreated {
			_ = json.NewEncoder(w).Encode(map[string]any{"enrollment_token": "agenr_minted"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "agent enrollment authorization failed"})
	}))
	t.Cleanup(coordinator.Close)

	var running atomic.Bool
	running.Store(true)
	var registered atomic.Bool
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !running.Load() {
			http.Error(w, "stopped", http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case r.Method == http.MethodGet && r.URL.Path == tunnel.AgentConnectionPath:
			status := tunnel.AgentConnectionStatus{Prepared: true, State: "prepared"}
			if registered.Load() {
				status = tunnel.AgentConnectionStatus{Prepared: true, Registered: true, Connected: true, State: "connected",
					AgentID: "agt_1", Label: "ci-runner", OrganizationID: "org_key", CoordinatorURL: coordinator.URL}
			}
			_ = json.NewEncoder(w).Encode(status)
		case r.Method == http.MethodPost && r.URL.Path == tunnel.AgentConnectionPath+"/prepare":
			_ = json.NewEncoder(w).Encode(tunnel.AgentConnectionStatus{Prepared: true, State: "prepared", PublicKeyFingerprint: "sha256:machine"})
		case r.Method == http.MethodPost && r.URL.Path == tunnel.AgentConnectionPath:
			var request tunnel.AgentConnectionRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			fixture.mu.Lock()
			fixture.redeemedTokens = append(fixture.redeemedTokens, request.EnrollmentToken)
			fixture.mu.Unlock()
			registered.Store(true)
			_ = json.NewEncoder(w).Encode(tunnel.AgentConnectionStatus{Registered: true, AgentID: "agt_1", Label: "ci-runner"})
		case r.Method == http.MethodPost && r.URL.Path == tunnel.ShutdownPath:
			running.Store(false)
			_ = json.NewEncoder(w).Encode(tunnel.ShutdownResponse{Stopping: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	t.Setenv("BEAM_API_URL", api.URL)
	t.Setenv("BEAM_AUTH_URL", api.URL)
	t.Setenv("BEAM_COORDINATOR_URL", coordinator.URL)
	t.Setenv("BEAM_API_KEY", "")
	fixture.app = New(version.BuildInfo{Version: "test"})
	// File-only credentials keep tests away from the developer's keychain.
	fixture.app.authStore = func(path string) auth.Store { return auth.Store{Path: path} }
	fixture.app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		running.Store(true)
		return tunnel.StartResult{AlreadyRunning: true}, nil
	}
	return fixture
}

func (f *apiKeyEnrollmentFixture) run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := f.app.Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func assertKeyNotPrinted(t *testing.T, outputs ...string) {
	t.Helper()
	for _, text := range outputs {
		if strings.Contains(text, testAPIKey) {
			t.Fatalf("output leaked the API key: %q", text)
		}
	}
}

func TestAgentConnectWithAPIKeyMintsEnrollmentWithoutLogin(t *testing.T) {
	fixture := newAPIKeyEnrollmentFixture(t)
	code, stdout, stderr := fixture.run("agent", "connect", "--api-key", testAPIKey, "--label", "ci-runner")
	if code != ExitOK {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	assertKeyNotPrinted(t, stdout, stderr)
	if fixture.verifyCalls != 1 {
		t.Fatalf("verify calls=%d, want 1 to discover the key's organization", fixture.verifyCalls)
	}
	if got := fixture.enrollHeaders.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Fatalf("Authorization=%q", got)
	}
	if got := fixture.enrollHeaders.Get("X-Beam-Organization-ID"); got != "org_key" {
		t.Fatalf("X-Beam-Organization-ID=%q", got)
	}
	if fixture.enrollBody["label"] != "ci-runner" || fixture.enrollBody["public_key_fingerprint"] != "sha256:machine" {
		t.Fatalf("enrollment body=%v", fixture.enrollBody)
	}
	if len(fixture.redeemedTokens) != 1 || fixture.redeemedTokens[0] != "agenr_minted" {
		t.Fatalf("daemon redeemed %v", fixture.redeemedTokens)
	}
	if !strings.Contains(stderr, "organization org_key with a Beam API key") || !strings.Contains(stdout, "agt_1") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	cfg, _, err := config.Load()
	if err != nil || cfg.Organization != "org_key" {
		t.Fatalf("saved organization=%q err=%v", cfg.Organization, err)
	}
}

func TestAgentConnectReadsAPIKeyFromEnvironment(t *testing.T) {
	fixture := newAPIKeyEnrollmentFixture(t)
	t.Setenv("BEAM_API_KEY", testAPIKey)
	code, stdout, stderr := fixture.run("agent", "connect", "--organization", "org_key", "--label", "ci-runner", "--json")
	if code != ExitOK {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	assertKeyNotPrinted(t, stdout, stderr)
	if fixture.verifyCalls != 1 {
		t.Fatalf("verify calls=%d, want 1: the key is checked even with --organization", fixture.verifyCalls)
	}
	if got := fixture.enrollHeaders.Get("X-Beam-Organization-ID"); got != "org_key" {
		t.Fatalf("X-Beam-Organization-ID=%q", got)
	}
}

// cacheOrganizations writes the organization list a `beam login` leaves
// behind, so --organization can name an organization by slug.
func cacheOrganizations(t *testing.T, organizations ...auth.Organization) {
	t.Helper()
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := (auth.Store{Path: paths.Credentials}).Save(auth.Credentials{RefreshToken: "refresh", Organizations: organizations}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentConnectAPIKeyResolvesCachedOrganizationSlug(t *testing.T) {
	fixture := newAPIKeyEnrollmentFixture(t)
	cacheOrganizations(t, auth.Organization{ID: "org_key", Slug: "kanjo"})
	code, stdout, stderr := fixture.run("agent", "connect", "--api-key", testAPIKey, "--organization", "kanjo", "--label", "ci-runner")
	if code != ExitOK {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if got := fixture.enrollHeaders.Get("X-Beam-Organization-ID"); got != "org_key" {
		t.Fatalf("X-Beam-Organization-ID=%q", got)
	}
}

func TestAgentConnectOnRegisteredMachineAcceptsOrganizationSlug(t *testing.T) {
	fixture := newAPIKeyEnrollmentFixture(t)
	if code, stdout, stderr := fixture.run("agent", "connect", "--api-key", testAPIKey, "--label", "ci-runner"); code != ExitOK {
		t.Fatalf("first connect code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	cacheOrganizations(t, auth.Organization{ID: "org_key", Slug: "kanjo"}, auth.Organization{ID: "org_other", Slug: "elsewhere"})
	code, stdout, stderr := fixture.run("agent", "connect", "--organization", "kanjo")
	if code != ExitOK || !strings.Contains(stdout, "agt_1") {
		t.Fatalf("registered organization slug: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	code, stdout, stderr = fixture.run("agent", "connect", "--organization", "elsewhere")
	if code != ExitConflict || !strings.Contains(stderr, "This machine is registered with another organization.") {
		t.Fatalf("another organization slug: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

func TestAgentConnectAPIKeyOrganizationGuard(t *testing.T) {
	for _, test := range []struct {
		name     string
		cached   []auth.Organization
		args     []string
		wantText []string
		// denyText must not appear: an organization ID gains nothing from a
		// login, so the slug sentence is left out for one.
		denyText []string
	}{
		{
			// Without a login there is no slug to compare: the Beam API reports
			// only the key's organization ID. The CLI must not send the slug to
			// the coordinator and blame the key.
			name: "slug without a login", args: []string{"--organization", "kanjo"},
			wantText: []string{`Could not match organization "kanjo"`, "belongs to organization org_key", "Pass that organization ID", "login` on this machine"},
		},
		{
			name: "unknown organization ID without a login", args: []string{"--organization", "org_other"},
			wantText: []string{`Could not match organization "org_other"`, "belongs to organization org_key"},
			denyText: []string{"slugs and public IDs", "login` on this machine"},
		},
		{
			name: "unknown cuid organization ID without a login", args: []string{"--organization", "cmf3k2x9q0000abcd1234efgh"},
			wantText: []string{`Could not match organization "cmf3k2x9q0000abcd1234efgh"`, "Pass that organization ID"},
			denyText: []string{"slugs and public IDs", "login` on this machine"},
		},
		{
			name: "public ID without a login", args: []string{"--organization", "A1b2C3d4E5f6"},
			wantText: []string{`Could not match organization "A1b2C3d4E5f6"`, "slugs and public IDs are resolved only after", "login` on this machine"},
		},
		{
			name: "cached slug of another organization", cached: []auth.Organization{{ID: "org_key", Slug: "kanjo"}, {ID: "org_other", Slug: "other"}},
			args:     []string{"--organization", "other"},
			wantText: []string{"does not belong to organization other", "belongs to organization org_key", "Omit --organization"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAPIKeyEnrollmentFixture(t)
			if len(test.cached) != 0 {
				cacheOrganizations(t, test.cached...)
			}
			args := append([]string{"agent", "connect", "--api-key", testAPIKey, "--label", "ci-runner"}, test.args...)
			code, stdout, stderr := fixture.run(args...)
			if code != ExitAuth {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			assertKeyNotPrinted(t, stdout, stderr)
			for _, want := range test.wantText {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr=%q, want %q", stderr, want)
				}
			}
			for _, deny := range test.denyText {
				if strings.Contains(stderr, deny) {
					t.Fatalf("stderr=%q, must not contain %q", stderr, deny)
				}
			}
			if fixture.enrollHeaders != nil {
				t.Fatalf("coordinator was asked to enroll into an unconfirmed organization: %v", fixture.enrollHeaders)
			}
		})
	}
}

func TestAgentConnectAPIKeyOutOfCreditStillIdentifiesOrganization(t *testing.T) {
	fixture := newAPIKeyEnrollmentFixture(t)
	fixture.verifyStatus = http.StatusPaymentRequired
	fixture.verifyBody = map[string]any{"valid": false, "error": "No credits remaining", "keyId": "key_1", "organizationId": "org_key"}
	code, stdout, stderr := fixture.run("agent", "connect", "--api-key", testAPIKey, "--label", "ci-runner")
	if code != ExitOK {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if got := fixture.enrollHeaders.Get("X-Beam-Organization-ID"); got != "org_key" {
		t.Fatalf("X-Beam-Organization-ID=%q", got)
	}
}

func TestAgentConnectAPIKeyErrors(t *testing.T) {
	for _, test := range []struct {
		name         string
		args         []string
		verifyStatus int
		verifyBody   map[string]any
		enrollStatus int
		wantCode     int
		wantText     []string
	}{
		{
			name: "key without billable permission", verifyStatus: http.StatusForbidden,
			verifyBody: map[string]any{"valid": false, "error": "API key is not allowed to run billable actions"},
			wantCode:   ExitAuth, wantText: []string{"cannot enroll this machine", "not allowed to run billable actions", "rooms:start"},
		},
		{
			name: "unknown key", verifyStatus: http.StatusUnauthorized, verifyBody: map[string]any{"valid": false, "error": "Invalid API key"},
			wantCode: ExitAuth, wantText: []string{"cannot enroll this machine", "Invalid API key"},
		},
		{
			// A revoked key reports the Beam API's reason with --organization
			// too, not only the coordinator's generic rejection.
			name: "revoked key with organization", args: []string{"--organization", "org_key"}, verifyStatus: http.StatusForbidden,
			verifyBody: map[string]any{"valid": false, "error": "API key has been revoked", "status": "REVOKED"},
			wantCode:   ExitAuth, wantText: []string{"cannot enroll this machine", "API key has been revoked"},
		},
		{
			name: "coordinator rejects key", args: []string{"--organization", "org_key"}, enrollStatus: http.StatusUnauthorized,
			wantCode: ExitAuth, wantText: []string{"coordinator rejected the Beam API key", "billable action"},
		},
		{
			name: "coordinator refuses organization", args: []string{"--organization", "org_key"}, enrollStatus: http.StatusForbidden,
			wantCode: ExitAuth, wantText: []string{"does not belong to organization org_key", "Omit --organization"},
		},
		{
			name: "combined with enrollment token", args: []string{"--enrollment-token", "agenr_x"},
			wantCode: ExitUsage, wantText: []string{"cannot be combined"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAPIKeyEnrollmentFixture(t)
			if test.verifyStatus != 0 {
				fixture.verifyStatus, fixture.verifyBody = test.verifyStatus, test.verifyBody
			}
			if test.enrollStatus != 0 {
				fixture.enrollStatus = test.enrollStatus
			}
			args := append([]string{"agent", "connect", "--api-key", testAPIKey, "--label", "ci-runner"}, test.args...)
			code, stdout, stderr := fixture.run(args...)
			if code != test.wantCode {
				t.Fatalf("code=%d want=%d stdout=%s stderr=%s", code, test.wantCode, stdout, stderr)
			}
			assertKeyNotPrinted(t, stdout, stderr)
			for _, want := range test.wantText {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr=%q, want %q", stderr, want)
				}
			}
			if len(fixture.redeemedTokens) != 0 {
				t.Fatalf("daemon redeemed %v after a failed enrollment", fixture.redeemedTokens)
			}
		})
	}
}

func TestSetupIgnoresAPIKeyEnvironment(t *testing.T) {
	// `beam setup --mode account` keeps the account flow: an exported
	// BEAM_API_KEY (used for rooms) must not silently change how it enrolls.
	fixture := newAPIKeyEnrollmentFixture(t)
	t.Setenv("BEAM_API_KEY", testAPIKey)
	code, stdout, stderr := fixture.run("setup", "--mode", "account", "--no-interactive")
	if code == ExitOK {
		t.Fatalf("setup without login succeeded: stdout=%s", stdout)
	}
	if fixture.enrollHeaders != nil {
		t.Fatalf("setup used the API key: headers=%v", fixture.enrollHeaders)
	}
	if !strings.Contains(stderr, "login") {
		t.Fatalf("stderr=%q, want a login requirement", stderr)
	}
}

func TestAgentConnectLoginHintMentionsNoInteractiveOnlyWhenPassed(t *testing.T) {
	for _, test := range []struct {
		name           string
		args           []string
		wantNoInteract bool
	}{
		{name: "not a terminal", args: []string{"agent", "connect", "--label", "ci-runner"}},
		{name: "flag passed", args: []string{"agent", "connect", "--label", "ci-runner", "--no-interactive"}, wantNoInteract: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAPIKeyEnrollmentFixture(t)
			code, stdout, stderr := fixture.run(test.args...)
			if code != ExitAuth || !strings.Contains(stderr, "Beam login is required") {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if got := strings.Contains(stderr, "--no-interactive"); got != test.wantNoInteract {
				t.Fatalf("hint mentions --no-interactive=%t, want %t: %q", got, test.wantNoInteract, stderr)
			}
		})
	}
}
