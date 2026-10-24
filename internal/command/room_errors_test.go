//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestRoomCreateExplainsCoordinatorRefusals(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     map[string]any
		wantCode int
		wantKind string
		wantText []string
	}{
		{
			name: "invitation-only agent", status: http.StatusForbidden,
			body:     map[string]any{"error": map[string]any{"code": "permission_denied", "message": "creating a room requires an agent connected through a Beam account"}},
			wantCode: ExitAuth, wantKind: "room_create_requires_account_agent",
			wantText: []string{"joined through a Room invitation", "`beam agent disconnect`", "`beam agent connect`"},
		},
		{
			name: "expired key", status: http.StatusUnauthorized,
			body:     map[string]any{"error": map[string]any{"code": "api_key_invalid", "message": "API key is expired"}},
			wantCode: ExitAuth, wantKind: "room_api_key_invalid",
			wantText: []string{"unknown, expired, revoked or disabled", "Renew or replace the key in the Beam Console"},
		},
		{
			name: "key without credit", status: http.StatusPaymentRequired,
			body: map[string]any{"error": map[string]any{
				"code": "insufficient_credits", "message": "Organization has no credits remaining",
				"details": map[string]any{"credits_required": 1, "credits_available": 0, "reason": "pool_exhausted"},
			}},
			wantCode: ExitOperationFailed, wantKind: "room_insufficient_credits",
			wantText: []string{"credits required: 1, available: 0; reason: pool_exhausted", "Add credits to the organization"},
		},
		{
			name: "key without rooms:start", status: http.StatusForbidden,
			body: map[string]any{"error": map[string]any{
				"code": "permission_required", "message": "API key is missing the rooms:start permission",
				"details": map[string]any{"permission": "rooms:start"},
			}},
			wantCode: ExitAuth, wantKind: "room_permission_required",
			wantText: []string{"rooms:start permission", "role grants the permission"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/rooms" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(test.body)
			}))
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			app := New(version.BuildInfo{Version: "test"})
			var stdout, stderr bytes.Buffer
			code := app.Run(context.Background(), []string{"room", "create", "--api-key", "b1m_secret"}, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("code=%d want=%d stderr=%s", code, test.wantCode, stderr.String())
			}
			for _, want := range test.wantText {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr=%q, want %q", stderr.String(), want)
				}
			}
			if strings.Contains(stderr.String(), "b1m_secret") {
				t.Fatalf("stderr leaked the API key: %q", stderr.String())
			}

			stdout.Reset()
			stderr.Reset()
			app.Run(context.Background(), []string{"room", "create", "--api-key", "b1m_secret", "--json"}, &stdout, &stderr)
			var payload struct {
				Error struct {
					Kind string `json:"kind"`
					Hint string `json:"hint"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil || payload.Error.Kind != test.wantKind || payload.Error.Hint == "" {
				t.Fatalf("json error=%s err=%v", stderr.String(), err)
			}
		})
	}
}

func TestRoomJoinExplainsInvitationRefusals(t *testing.T) {
	for _, test := range []struct {
		name       string
		registered bool
		status     int
		code       string
		reason     string
		wantCode   int
		wantKind   string
		wantText   string
	}{
		{name: "registered agent, expired", registered: true, status: http.StatusGone, code: "expired", reason: "invitation_expired",
			wantCode: ExitAuth, wantKind: "invitation_expired", wantText: "This Room invitation has expired."},
		{name: "registered agent, invalid", registered: true, status: http.StatusForbidden, code: "permission_denied", reason: "invitation_invalid",
			wantCode: ExitAuth, wantKind: "invitation_invalid", wantText: "not valid for this Room"},
		{name: "registered agent, denied without a reason", registered: true, status: http.StatusForbidden, code: "permission_denied",
			wantCode: ExitAuth, wantKind: "invitation_invalid", wantText: "Check the Room ID and the invitation token"},
		{name: "registered agent, used up", registered: true, status: http.StatusConflict, code: "replayed", reason: "invitation_exhausted",
			wantCode: ExitConflict, wantKind: "invitation_exhausted", wantText: "used up"},
		{name: "new machine, revoked", status: http.StatusConflict, code: "replayed", reason: "invitation_revoked",
			wantCode: ExitAuth, wantKind: "invitation_revoked", wantText: "The Room owner revoked this invitation."},
		{name: "new machine, room closed", status: http.StatusConflict, code: "conflict", reason: "room_closed",
			wantCode: ExitConflict, wantKind: "room_closed", wantText: "This Room is closed"},
		{name: "new machine, not found", status: http.StatusNotFound, code: "not_found", reason: "invitation_not_found",
			wantCode: ExitNotFound, wantKind: "invitation_not_found", wantText: "was not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			refusal := map[string]any{"code": test.code, "message": "BTR invitation refused", "retryable": false}
			if test.reason != "" {
				refusal["details"] = map[string]any{"reason": test.reason}
			}
			joinPath := "/v1/agent-connection/room-invitation"
			if test.registered {
				joinPath = "/v1/rooms/btr_room_a/join"
			}
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/agent-connection":
					_ = json.NewEncoder(w).Encode(map[string]any{"prepared": true, "registered": test.registered,
						"coordinator_url": "https://coordinator.example.test"})
				case r.Method == http.MethodPost && r.URL.Path == joinPath:
					w.WriteHeader(test.status)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": refusal})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			app := New(version.BuildInfo{Version: "test"})
			args := []string{"room", "join", "btr_room_a", "--invitation-token", "btri_secret"}
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), args, &stdout, &stderr); code != test.wantCode {
				t.Fatalf("code=%d want=%d stderr=%s", code, test.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.wantText) || strings.Contains(stderr.String(), "manage") ||
				strings.Contains(stderr.String(), "btri_secret") {
				t.Fatalf("stderr=%q, want %q without a manage hint or the token", stderr.String(), test.wantText)
			}

			stdout.Reset()
			stderr.Reset()
			app.Run(context.Background(), append(args, "--json"), &stdout, &stderr)
			var payload struct {
				Error struct {
					Code int    `json:"code"`
					Kind string `json:"kind"`
					Hint string `json:"hint"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil || payload.Error.Kind != test.wantKind ||
				payload.Error.Code != test.wantCode || payload.Error.Hint == "" {
				t.Fatalf("json error=%s err=%v", stderr.String(), err)
			}
		})
	}
}

func TestRoomChannelHelpExplainsLiveOnlyRetention(t *testing.T) {
	for _, args := range [][]string{
		{"room", "channel", "publish", "--help"},
		{"room", "btr_room_a", "channel", "btr_channel_a", "publish", "--help"},
		{"room", "btr_room_a", "channel", "btr_channel_a", "listen", "--help"},
		{"help", "room"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			var stdout, stderr bytes.Buffer
			if code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "--retention none, which is live only") {
				t.Fatalf("help does not explain live-only retention:\n%s", stdout.String())
			}
		})
	}
}

func TestRoomPublishExplainsWorkerTimeout(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/publish") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusGatewayTimeout)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "channel_setup_timeout", "retryable": true,
			"message": "room workload provisioning timed out after 30s (room=btr_room_a channel=btr_channel_a workload=btr_wl_1 last_state=submitted)",
		}})
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	app := New(version.BuildInfo{Version: "test"})
	args := []string{"room", "channel", "publish", "btr_room_a", "btr_channel_a", "--literal", "hi"}
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), args, &stdout, &stderr); code != ExitOperationFailed {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"no Beam delivery Worker picked it up in time", "no Worker available"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr=%q, want %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "agent logs") {
		t.Fatalf("stderr still points at the agent logs: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	app.Run(context.Background(), append(args, "--json"), &stdout, &stderr)
	var payload struct {
		Error struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil || payload.Error.Kind != "channel_setup_timeout" {
		t.Fatalf("json error=%s err=%v", stderr.String(), err)
	}
}
