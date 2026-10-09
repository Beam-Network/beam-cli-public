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
			name: "key without rooms:start", status: http.StatusPaymentRequired,
			body:     map[string]any{"error": map[string]any{"code": "CREDIT_REQUIRED", "message": "API key is missing the rooms:start permission"}},
			wantCode: ExitOperationFailed, wantKind: "room_api_key_permission",
			wantText: []string{"rooms:start permission", "role grants rooms:start"},
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
