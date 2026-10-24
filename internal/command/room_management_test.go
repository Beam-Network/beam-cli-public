//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestScopedRoomManagementCommandsUseDaemonRoutes(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		method string
		path   string
	}{
		{name: "invitation list", args: []string{"room", "room-a", "invitation", "list"}, method: http.MethodGet, path: "/v1/rooms/room-a/invitations"},
		{name: "invitation show", args: []string{"room", "room-a", "invitation", "show", "invite-a"}, method: http.MethodGet, path: "/v1/rooms/room-a/invitations/invite-a"},
		{name: "invitation revoke", args: []string{"room", "room-a", "invitation", "revoke", "invite-a"}, method: http.MethodDelete, path: "/v1/rooms/room-a/invitations/invite-a"},
		{name: "member remove", args: []string{"room", "room-a", "member", "remove", "member-a"}, method: http.MethodDelete, path: "/v1/rooms/room-a/memberships/member-a"},
		{name: "role delete", args: []string{"room", "room-a", "role", "delete", "role-a"}, method: http.MethodDelete, path: "/v1/rooms/room-a/roles/role-a"},
		{name: "media stop", args: []string{"room", "room-a", "channel", "channel-a", "media", "stop", "workload-a"}, method: http.MethodPost, path: "/v1/rooms/room-a/channels/channel-a/workloads/workload-a/cancel"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			var mediaStopKeys []string
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == tunnel.StatusPath {
					_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
					return
				}
				if request.Method != test.method || request.URL.Path != test.path {
					http.NotFound(w, request)
					return
				}
				if test.name == "media stop" {
					mediaStopKeys = append(mediaStopKeys, request.Header.Get("Idempotency-Key"))
				}
				called = true
				if test.name == "invitation list" {
					_, _ = w.Write([]byte(`{"invitations":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			var stdout, stderr bytes.Buffer
			if code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), test.args, &stdout, &stderr); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if !called {
				t.Fatalf("daemon route %s %s was not called", test.method, test.path)
			}
			if test.name == "media stop" {
				stdout.Reset()
				stderr.Reset()
				if code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), test.args, &stdout, &stderr); code != ExitOK {
					t.Fatalf("repeat code=%d stderr=%s", code, stderr.String())
				}
				if len(mediaStopKeys) != 2 || mediaStopKeys[0] == "" || mediaStopKeys[0] != mediaStopKeys[1] {
					t.Fatalf("media stop keys not stable and nonempty: %q", mediaStopKeys)
				}
			}
		})
	}
}
