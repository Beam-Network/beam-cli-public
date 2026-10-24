//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const (
	testWebhookSecret = "s3cr3t-signing-secret-value"
	testWebhookURL    = "https://hooks.example.com/beam?token=url-borne-token"
)

var allWebhookCapabilities = []string{webhookManageCapability, webhookHostCapability}

type webhookCall struct {
	Method string
	Path   string
	Key    string
	Body   map[string]any
}

// webhookDaemon fakes the local agent: /v1/status advertises capabilities,
// and every other request is recorded and answered by respond.
type webhookDaemon struct {
	mu    sync.Mutex
	calls []webhookCall
}

func (daemon *webhookDaemon) routeCalls() []webhookCall {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	return append([]webhookCall(nil), daemon.calls...)
}

func startWebhookDaemon(t *testing.T, capabilities []string, respond func(method, path string) (int, any)) *webhookDaemon {
	t.Helper()
	daemon := &webhookDaemon{}
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == tunnel.StatusPath {
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion,
				ProtocolMinVersion: tunnel.ProtocolMinVersion, Capabilities: capabilities})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		daemon.mu.Lock()
		daemon.calls = append(daemon.calls, webhookCall{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key"), Body: body})
		daemon.mu.Unlock()
		status, payload := respond(r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	return daemon
}

func sampleWebhook(overrides map[string]any) map[string]any {
	webhook := map[string]any{
		"member_id": "btr_member_w", "room_id": "btr_room_a", "host_member_id": "btr_member_h",
		"state": "pending", "payload_mode": "envelope", "max_file_bytes": 25 << 20, "version": 1,
	}
	for key, value := range overrides {
		webhook[key] = value
	}
	return webhook
}

// webhookResponses answers each daemon route with its documented shape.
func webhookResponses(method, path string) (int, any) {
	switch {
	case path == "/v1/hosted-webhooks" || path == "/v1/rooms/btr_room_a/webhooks" && method == http.MethodGet:
		return http.StatusOK, map[string]any{"webhooks": []any{sampleWebhook(nil)}}
	case path == "/v1/rooms/btr_room_a/webhooks":
		return http.StatusCreated, map[string]any{"webhook": sampleWebhook(nil)}
	case method == http.MethodDelete:
		return http.StatusOK, map[string]any{"removed": true, "room_id": "btr_room_a", "member_id": "btr_member_w"}
	case strings.HasSuffix(path, "/decline"):
		return http.StatusOK, map[string]any{"declined": true, "room_id": "btr_room_a", "member_id": "btr_member_w"}
	case strings.HasSuffix(path, "/test"):
		return http.StatusOK, map[string]any{"test": map[string]any{"class": "delivered", "http_status": 200, "attempts": 1, "reply": false}}
	case strings.HasSuffix(path, "/accept"), strings.HasSuffix(path, "/secret"):
		return http.StatusOK, map[string]any{"webhook": sampleWebhook(map[string]any{"state": "active", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"})}
	default:
		return http.StatusOK, map[string]any{"webhook": sampleWebhook(nil)}
	}
}

func newWebhookApp() *App {
	app := New(version.BuildInfo{Version: "test"})
	app.stdinTerminal = func() bool { return false }
	app.stdin = strings.NewReader("")
	return app
}

func runWebhookCLI(app *App, args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func writeSecretFileForTest(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRoomWebhookCommandsUseDaemonRoutes(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		method  string
		path    string
		wantKey bool
		body    map[string]any
	}{
		{
			name: "add",
			args: []string{"room", "btr_room_a", "webhook", "add", "--name", "Alerts", "--host", "btr_member_h",
				"--channel", "btr_chan_1=discover,subscribe,subscribe", "--channel=btr_chan_2=Publish", "--payload-mode", "raw", "--max-file-mb", "10"},
			method: http.MethodPost, path: "/v1/rooms/btr_room_a/webhooks", wantKey: true,
			body: map[string]any{
				"display_name": "Alerts", "host_member_id": "btr_member_h", "payload_mode": "raw", "max_file_bytes": float64(10 << 20),
				"grants": []any{
					map[string]any{"channel_id": "btr_chan_1", "actions": []any{"discover", "subscribe"}},
					map[string]any{"channel_id": "btr_chan_2", "actions": []any{"publish"}},
				},
			},
		},
		{name: "list", args: []string{"room", "btr_room_a", "webhook", "list"}, method: http.MethodGet, path: "/v1/rooms/btr_room_a/webhooks"},
		{name: "list by default", args: []string{"room", "btr_room_a", "webhook"}, method: http.MethodGet, path: "/v1/rooms/btr_room_a/webhooks"},
		{name: "show", args: []string{"room", "btr_room_a", "webhook", "show", "btr_member_w"}, method: http.MethodGet, path: "/v1/rooms/btr_room_a/webhooks/btr_member_w"},
		{name: "remove", args: []string{"room", "btr_room_a", "webhook", "remove", "btr_member_w"}, method: http.MethodDelete, path: "/v1/rooms/btr_room_a/webhooks/btr_member_w", wantKey: true},
		{name: "resume", args: []string{"room", "btr_room_a", "webhook", "resume", "btr_member_w"}, method: http.MethodPost, path: "/v1/rooms/btr_room_a/webhooks/btr_member_w/resume", wantKey: true},
		{
			name: "move", args: []string{"room", "btr_room_a", "webhook", "move", "btr_member_w", "--host", "btr_member_x"},
			method: http.MethodPost, path: "/v1/rooms/btr_room_a/webhooks/btr_member_w/host", wantKey: true,
			body: map[string]any{"host_member_id": "btr_member_x"},
		},
		{name: "decline", args: []string{"room", "btr_room_a", "webhook", "decline", "btr_member_w"}, method: http.MethodPost, path: "/v1/hosted-webhooks/btr_room_a/btr_member_w/decline"},
		{name: "test", args: []string{"room", "btr_room_a", "webhook", "test", "btr_member_w"}, method: http.MethodPost, path: "/v1/hosted-webhooks/btr_room_a/btr_member_w/test"},
		{name: "hosted", args: []string{"webhook", "hosted"}, method: http.MethodGet, path: "/v1/hosted-webhooks"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
			for _, mode := range []string{"", "--json"} {
				args := test.args
				if mode != "" {
					args = append(append([]string(nil), args...), mode)
				}
				stdout, stderr, code := runWebhookCLI(newWebhookApp(), args...)
				if code != ExitOK {
					t.Fatalf("%v: code=%d stderr=%s", args, code, stderr)
				}
				if mode == "--json" && !json.Valid([]byte(stdout)) {
					t.Fatalf("--json printed invalid JSON: %q", stdout)
				}
			}
			calls := daemon.routeCalls()
			if len(calls) != 2 {
				t.Fatalf("calls = %+v, want one per run", calls)
			}
			for _, call := range calls {
				if call.Method != test.method || call.Path != test.path {
					t.Fatalf("called %s %s, want %s %s", call.Method, call.Path, test.method, test.path)
				}
				if test.wantKey != (call.Key != "") {
					t.Fatalf("Idempotency-Key = %q, want present=%v", call.Key, test.wantKey)
				}
				if test.body != nil && !reflect.DeepEqual(call.Body, test.body) {
					t.Fatalf("body = %#v\nwant %#v", call.Body, test.body)
				}
			}
			if test.wantKey && calls[0].Key == calls[1].Key {
				t.Fatalf("two separate invocations reused Idempotency-Key %q", calls[0].Key)
			}
		})
	}
}

func TestRoomWebhookAddUsesTheGivenIdempotencyKey(t *testing.T) {
	daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
	stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "add", "--name", "Alerts",
		"--host", "btr_member_h", "--idempotency-key", "alerts-1", "--quiet")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if stdout != "btr_member_w\n" {
		t.Fatalf("--quiet printed %q, want the webhook member ID", stdout)
	}
	calls := daemon.routeCalls()
	if len(calls) != 1 || calls[0].Key != "alerts-1" || !reflect.DeepEqual(calls[0].Body["grants"], []any{}) {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestRoomWebhookAcceptSendsTheSecretWithoutPrintingIt(t *testing.T) {
	secretFile := writeSecretFileForTest(t, testWebhookSecret+"\n", 0o600)
	for _, test := range []struct {
		name   string
		source []string
		stdin  string
	}{
		{name: "file", source: []string{"--secret-file", secretFile}},
		{name: "stdin", source: []string{"--secret-stdin"}, stdin: testWebhookSecret + "\r\n"},
	} {
		for _, mode := range []string{"", "--json", "--verbose"} {
			t.Run(test.name+mode, func(t *testing.T) {
				daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
				app := newWebhookApp()
				app.stdin = strings.NewReader(test.stdin)
				args := append([]string{"room", "btr_room_a", "webhook", "accept", "btr_member_w", "--url", testWebhookURL,
					"--allow-private", "--payload-mode", "envelope", "--max-file-mb", "5", "--yes"}, test.source...)
				if mode != "" {
					args = append(args, mode)
				}
				stdout, stderr, code := runWebhookCLI(app, args...)
				if code != ExitOK {
					t.Fatalf("code=%d stderr=%s", code, stderr)
				}
				calls := daemon.routeCalls()
				if len(calls) != 1 || calls[0].Path != "/v1/hosted-webhooks/btr_room_a/btr_member_w/accept" {
					t.Fatalf("calls = %+v", calls)
				}
				want := map[string]any{"url": testWebhookURL, "secret": testWebhookSecret, "allow_private": true,
					"payload_mode": "envelope", "max_file_bytes": float64(5 << 20)}
				if !reflect.DeepEqual(calls[0].Body, want) {
					t.Fatalf("body = %#v\nwant %#v", calls[0].Body, want)
				}
				for _, secret := range []string{testWebhookSecret, "url-borne-token"} {
					if strings.Contains(stdout+stderr, secret) {
						t.Fatalf("output contains %q:\nstdout=%s\nstderr=%s", secret, stdout, stderr)
					}
				}
				if mode == "" && !strings.Contains(stderr, "decrypts everything sent to the webhook") {
					t.Fatalf("the trust model was not shown: %q", stderr)
				}
			})
		}
	}
}

func TestRoomWebhookAcceptAsksToConfirmTheTrustModel(t *testing.T) {
	secretFile := writeSecretFileForTest(t, testWebhookSecret, 0o600)
	args := []string{"room", "btr_room_a", "webhook", "accept", "btr_member_w", "--url", testWebhookURL, "--secret-file", secretFile}

	t.Run("without a terminal", func(t *testing.T) {
		daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
		_, stderr, code := runWebhookCLI(newWebhookApp(), args...)
		if code != ExitUsage || !strings.Contains(stderr, "--yes") {
			t.Fatalf("code=%d stderr=%s", code, stderr)
		}
		if calls := daemon.routeCalls(); len(calls) != 0 {
			t.Fatalf("accepted without confirmation: %+v", calls)
		}
	})
	for answer, accepted := range map[string]bool{"n": false, "": false, "yes": true} {
		t.Run("answer "+answer, func(t *testing.T) {
			daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
			app := newWebhookApp()
			app.isTerminal = func(io.Writer) bool { return true }
			app.stdinTerminal = func() bool { return true }
			app.readLine = func() (string, error) { return answer, nil }
			_, stderr, code := runWebhookCLI(app, args...)
			if !strings.Contains(stderr, "Host this webhook on this machine?") {
				t.Fatalf("no confirmation prompt: %q", stderr)
			}
			calls := daemon.routeCalls()
			if accepted != (len(calls) == 1) || accepted != (code == ExitOK) {
				t.Fatalf("answer %q: code=%d calls=%+v stderr=%s", answer, code, calls, stderr)
			}
		})
	}
}

func TestRoomWebhookSecretIsNeverACommandLineValue(t *testing.T) {
	private := writeSecretFileForTest(t, testWebhookSecret, 0o600)
	shared := writeSecretFileForTest(t, testWebhookSecret, 0o644)
	short := writeSecretFileForTest(t, "too-short", 0o600)
	accept := []string{"room", "btr_room_a", "webhook", "accept", "btr_member_w", "--url", testWebhookURL, "--yes"}
	rotate := []string{"room", "btr_room_a", "webhook", "rotate-secret", "btr_member_w"}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "accept --secret", args: append(append([]string(nil), accept...), "--secret", testWebhookSecret), want: "--secret-file"},
		{name: "rotate --secret=", args: append(append([]string(nil), rotate...), "--secret="+testWebhookSecret), want: "--secret-stdin"},
		{name: "no source", args: accept, want: "exactly one of"},
		{name: "two sources", args: append(append([]string(nil), accept...), "--secret-file", private, "--secret-stdin"), want: "exactly one of"},
		{name: "shared file", args: append(append([]string(nil), rotate...), "--secret-file", shared), want: "chmod 600"},
		{name: "short secret", args: append(append([]string(nil), rotate...), "--secret-file", short), want: "16 to 512 bytes"},
		{name: "http URL", args: []string{"room", "btr_room_a", "webhook", "accept", "btr_member_w", "--url", "http://hooks.example.com", "--secret-file", private, "--yes"}, want: "https URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
			_, stderr, code := runWebhookCLI(newWebhookApp(), test.args...)
			if code != ExitUsage || !strings.Contains(stderr, test.want) {
				t.Fatalf("code=%d stderr=%s, want %q", code, stderr, test.want)
			}
			if strings.Contains(stderr, testWebhookSecret) {
				t.Fatalf("stderr echoes the secret: %q", stderr)
			}
			if calls := daemon.routeCalls(); len(calls) != 0 {
				t.Fatalf("daemon called: %+v", calls)
			}
		})
	}
}

func TestRoomWebhookRotateSecretOverlap(t *testing.T) {
	secretFile := writeSecretFileForTest(t, testWebhookSecret, 0o600)
	for _, test := range []struct {
		overlap []string
		want    any
	}{
		{overlap: nil, want: nil},
		{overlap: []string{"--overlap", "48h"}, want: float64(48 * 3600)},
		{overlap: []string{"--overlap", "0"}, want: float64(0)},
	} {
		daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
		args := append([]string{"room", "btr_room_a", "webhook", "rotate-secret", "btr_member_w", "--secret-file", secretFile}, test.overlap...)
		stdout, stderr, code := runWebhookCLI(newWebhookApp(), args...)
		if code != ExitOK {
			t.Fatalf("%v: code=%d stderr=%s", test.overlap, code, stderr)
		}
		calls := daemon.routeCalls()
		if len(calls) != 1 || calls[0].Path != "/v1/hosted-webhooks/btr_room_a/btr_member_w/secret" ||
			calls[0].Body["secret"] != testWebhookSecret || calls[0].Body["overlap_seconds"] != test.want {
			t.Fatalf("%v: calls = %+v", test.overlap, calls)
		}
		if strings.Contains(stdout+stderr, testWebhookSecret) {
			t.Fatalf("output contains the secret: %q %q", stdout, stderr)
		}
	}
	startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
	_, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "rotate-secret", "btr_member_w",
		"--secret-file", secretFile, "--overlap", "200h")
	if code != ExitUsage || !strings.Contains(stderr, "168h") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestRoomWebhookAddValidatesItsOptions(t *testing.T) {
	base := []string{"room", "btr_room_a", "webhook", "add", "--name", "Alerts", "--host", "btr_member_h"}
	for _, test := range []struct {
		extra []string
		want  string
	}{
		{extra: []string{"--channel", "btr_chan_1=manage"}, want: `"manage" is not allowed`},
		{extra: []string{"--channel", "btr_chan_1"}, want: "CHANNEL_ID=ACTION"},
		{extra: []string{"--channel", "btr_chan_1=publish", "--channel", "btr_chan_1=subscribe"}, want: "more than once"},
		{extra: []string{"--max-file-mb", "26"}, want: "1 to 25"},
		{extra: []string{"--payload-mode", "xml"}, want: "envelope or raw"},
	} {
		daemon := startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
		_, stderr, code := runWebhookCLI(newWebhookApp(), append(append([]string(nil), base...), test.extra...)...)
		if code != ExitUsage || !strings.Contains(stderr, test.want) {
			t.Fatalf("%v: code=%d stderr=%s, want %q", test.extra, code, stderr, test.want)
		}
		if calls := daemon.routeCalls(); len(calls) != 0 {
			t.Fatalf("%v: daemon called: %+v", test.extra, calls)
		}
	}
	startWebhookDaemon(t, allWebhookCapabilities, webhookResponses)
	if _, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "add", "--name", "Alerts"); code != ExitUsage || !strings.Contains(stderr, "--host is required") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestRoomWebhookCommandsNeedTheAgentCapability(t *testing.T) {
	for _, test := range []struct {
		capabilities []string
		args         []string
		wantKind     string
	}{
		{capabilities: nil, args: []string{"room", "btr_room_a", "webhook", "list"}, wantKind: "webhook_manage_unavailable"},
		{capabilities: []string{webhookHostCapability}, args: []string{"room", "btr_room_a", "webhook", "resume", "btr_member_w"}, wantKind: "webhook_manage_unavailable"},
		{capabilities: []string{webhookManageCapability}, args: []string{"room", "btr_room_a", "webhook", "test", "btr_member_w"}, wantKind: "webhook_host_unavailable"},
		{capabilities: []string{webhookManageCapability}, args: []string{"webhook", "hosted"}, wantKind: "webhook_host_unavailable"},
	} {
		daemon := startWebhookDaemon(t, test.capabilities, webhookResponses)
		_, stderr, code := runWebhookCLI(newWebhookApp(), append(test.args, "--json")...)
		var payload struct {
			Error struct {
				Kind string `json:"kind"`
				Hint string `json:"hint"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(stderr), &payload); err != nil || code != ExitOperationFailed || payload.Error.Kind != test.wantKind {
			t.Fatalf("%v: code=%d stderr=%s err=%v", test.args, code, stderr, err)
		}
		if !strings.Contains(payload.Error.Hint, "`beam update`") {
			t.Fatalf("%v: hint does not suggest an update: %q", test.args, payload.Error.Hint)
		}
		if calls := daemon.routeCalls(); len(calls) != 0 {
			t.Fatalf("%v: daemon called: %+v", test.args, calls)
		}
	}
}

func TestRoomWebhookErrorsAreExplained(t *testing.T) {
	add := []string{"room", "btr_room_a", "webhook", "add", "--name", "Alerts", "--host", "btr_member_h", "--channel", "btr_chan_1=subscribe"}
	secretFile := writeSecretFileForTest(t, testWebhookSecret, 0o600)
	accept := []string{"room", "btr_room_a", "webhook", "accept", "btr_member_w", "--url", testWebhookURL, "--secret-file", secretFile, "--yes"}
	member := func(action string) []string { return []string{"room", "btr_room_a", "webhook", action, "btr_member_w"} }
	for _, test := range []struct {
		name     string
		args     []string
		status   int
		code     string
		message  string
		wantExit int
		wantKind string
		wantText string
	}{
		{name: "not owner", args: add, status: 403, code: "permission_denied", message: "BTR authorization denied", wantExit: ExitAuth, wantKind: "webhook_permission_denied", wantText: "owner or admin"},
		{name: "ineligible host", args: add, status: 422, code: "unsupported_capability", message: "btr webhook host is not eligible", wantExit: ExitOperationFailed, wantKind: "webhook_host_ineligible", wantText: "beam room btr_room_a member list"},
		{name: "grant outside limits", args: add, status: 422, code: "unsupported_capability", message: "btr webhook grants are limited to Discover, Subscribe and Publish on message channels and Discover and Subscribe on object channels", wantExit: ExitOperationFailed, wantKind: "webhook_grant_not_allowed", wantText: "no manage"},
		{name: "over limits", args: add, status: 429, code: "resource_exhausted", message: "BTR control quota exceeded", wantExit: ExitOperationFailed, wantKind: "webhook_limit_reached", wantText: "beam room btr_room_a webhook remove"},
		{name: "manager must upgrade", args: add, status: 409, code: "webhook_controller_upgrade_required", message: "member(s) btr_member_m may control a channel this webhook uses and run an agent without webhook member support: they must upgrade beam", wantExit: ExitConflict, wantKind: "webhook_controller_upgrade_required", wantText: "btr_member_m"},
		{name: "manager must upgrade before accept", args: accept, status: 409, code: "webhook_controller_upgrade_required", message: "member(s) btr_member_m may control a channel this webhook uses and run an agent without webhook member support: they must upgrade beam", wantExit: ExitConflict, wantKind: "webhook_controller_upgrade_required", wantText: "beam update"},
		{name: "channel keeps messages", args: add, status: 422, code: "webhook_channel_requires_retention", message: "channel btr_chan_1 does not allow retention none", wantExit: ExitOperationFailed, wantKind: "webhook_channel_requires_retention", wantText: "only live messages"},
		{name: "state conflict", args: member("resume"), status: 409, code: "conflict", message: "btr webhook state does not allow this change", wantExit: ExitConflict, wantKind: "webhook_state_conflict", wantText: "beam room btr_room_a webhook move btr_member_w --host"},
		{name: "unknown webhook", args: member("show"), status: 404, code: "not_found", message: "BTR resource not found", wantExit: ExitNotFound, wantKind: "webhook_not_found", wantText: "beam room btr_room_a webhook list"},
		{name: "not pending", args: accept, status: 409, code: "webhook_not_pending", message: "webhook is not waiting for this agent to accept it", wantExit: ExitConflict, wantKind: "webhook_not_pending", wantText: "beam webhook hosted"},
		{name: "not accepted", args: member("test"), status: 409, code: "webhook_not_accepted", message: "webhook is not accepted on this agent", wantExit: ExitConflict, wantKind: "webhook_not_accepted", wantText: "beam room btr_room_a webhook accept btr_member_w"},
		{name: "not hosted", args: member("decline"), status: 404, code: "webhook_not_hosted", message: "webhook is not hosted by this agent", wantExit: ExitNotFound, wantKind: "webhook_not_hosted", wantText: "beam webhook hosted"},
		{name: "join-only host", args: accept, status: 403, code: "webhook_host_ineligible", message: "only an agent enrolled with a Beam account can host webhooks", wantExit: ExitAuth, wantKind: "webhook_host_requires_account_agent", wantText: "connected to a Beam account"},
		{name: "blocked URL", args: accept, status: 400, code: "invalid_request", message: "webhook URL host cannot be used: blocked address", wantExit: ExitOperationFailed, wantKind: "webhook_invalid_request", wantText: "--allow-private"},
		{name: "no host support", args: []string{"webhook", "hosted"}, status: 503, code: "webhook_hosting_unavailable", message: "Webhook hosting needs an account-enrolled agent", wantExit: ExitOperationFailed, wantKind: "webhook_host_unavailable", wantText: "beam agent connect"},
		{name: "no coordinator support", args: member("show"), status: 503, code: "room_webhooks_unavailable", message: "Room webhook members need a coordinator that supports them.", wantExit: ExitOperationFailed, wantKind: "webhook_manage_unavailable", wantText: "room-webhooks-v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			startWebhookDaemon(t, allWebhookCapabilities, func(string, string) (int, any) {
				return test.status, map[string]any{"error": map[string]any{"code": test.code, "message": test.message}}
			})
			_, stderr, code := runWebhookCLI(newWebhookApp(), append(append([]string(nil), test.args...), "--json")...)
			var payload struct {
				Error struct {
					Kind    string `json:"kind"`
					Message string `json:"message"`
					Hint    string `json:"hint"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
				t.Fatalf("stderr is not a JSON error: %q", stderr)
			}
			if code != test.wantExit || payload.Error.Kind != test.wantKind {
				t.Fatalf("code=%d kind=%q, want %d %q (stderr=%s)", code, payload.Error.Kind, test.wantExit, test.wantKind, stderr)
			}
			if !strings.Contains(payload.Error.Message+" "+payload.Error.Hint, test.wantText) {
				t.Fatalf("error does not mention %q: %+v", test.wantText, payload.Error)
			}
			if strings.Contains(stderr, testWebhookSecret) {
				t.Fatalf("error echoes the secret: %q", stderr)
			}
		})
	}
}

func TestRoomWebhookShowExplainsTheState(t *testing.T) {
	for _, test := range []struct {
		webhook map[string]any
		want    []string
	}{
		{
			webhook: map[string]any{"state": "degraded", "reason": "auth", "host_agent_id": "agt_h", "presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want:    []string{"State:    degraded (auth)", "Host:     btr_member_h (agent agt_h)", "URL host: hooks.example.com (sha256:abc)", "Still delivering", "rejected the signature"},
		},
		{
			webhook: map[string]any{"state": "active", "host_agent_id": "agt_h", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want:    []string{"State:    active", "Host:     btr_member_h (agent agt_h), online", "Delivering."},
		},
		{
			webhook: map[string]any{"state": "active", "host_agent_id": "agt_h", "presence": "offline", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want: []string{"State:    active\n", "Host:     btr_member_h (agent agt_h), online",
				"Webhook offline: not delivering. Its host member btr_member_h is online, and delivery starts once the host's agent runs the webhook",
				"`beam webhook hosted`"},
		},
		{
			webhook: map[string]any{"state": "degraded", "reason": "failing", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want:    []string{"State:    degraded (failing)", "Webhook offline: not delivering."},
		},
		{
			webhook: map[string]any{"state": "active", "host_agent_id": "agt_h", "host_presence": "offline", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want: []string{"State:    active\n", "Host:     btr_member_h (agent agt_h), offline",
				"Host offline: not delivering. Delivery resumes when the agent of host member btr_member_h is back online"},
		},
		{
			webhook: map[string]any{"state": "degraded", "reason": "failing", "host_presence": "offline", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want:    []string{"State:    degraded (failing)", "Host offline: not delivering."},
		},
		{
			webhook: map[string]any{"state": "degraded", "reason": "failing", "host_agent_id": "agt_h", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want:    []string{"Still delivering, but delivery attempts failed repeatedly. Its host sends one attempt at a time until one succeeds."},
		},
		{
			webhook: map[string]any{"state": "paused", "reason": "failing", "host_agent_id": "agt_h", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want:    []string{"Not delivering: delivery attempts failed repeatedly. Resume it with:"},
		},
		{
			webhook: map[string]any{"state": "paused", "reason": "host_left", "host_member_id": ""},
			want:    []string{"State:    paused (host_left)", "Host:     none", "its host left the Room", "beam room btr_room_a webhook move btr_member_w --host <member-id>"},
		},
		{
			webhook: map[string]any{"state": "paused", "reason": "gone", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"},
			want:    []string{"HTTP 410", "beam room btr_room_a webhook resume btr_member_w"},
		},
		{
			webhook: map[string]any{},
			want:    []string{"URL host: not entered yet", "Payload:  envelope, files up to 25 MiB", "beam room btr_room_a webhook accept btr_member_w"},
		},
		{
			webhook: map[string]any{"state": "active", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com",
				"url_fingerprint": "sha256:abc", "durable": true},
			want: []string{"Payload:  envelope, files up to 25 MiB\nDurable:  on, its host keeps undelivered events of channels that allow receiver_local, encrypted\n"},
		},
		{
			webhook: map[string]any{"state": "active", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com",
				"url_fingerprint": "sha256:abc", "durable": false},
			want: []string{"Durable:  off, live delivery only\n"},
		},
		{
			webhook: map[string]any{"state": "active", "host_agent_id": "agt_h", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com",
				"url_fingerprint": "sha256:abc", "keys_pending_channel_ids": []any{"btr_channel_e"}},
			want: []string{"State:    active\n", "Joining the encryption of 1 channel: not delivering on it yet (btr_channel_e).",
				"after an accept, a host move or another membership change takes a few seconds"},
		},
		{
			webhook: map[string]any{"state": "degraded", "reason": "failing", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com",
				"url_fingerprint": "sha256:abc", "keys_pending_channel_ids": []any{"btr_channel_e", "btr_channel_f"}},
			want: []string{"Joining the encryption of 2 channels: not delivering on them yet (btr_channel_e, btr_channel_f)."},
		},
		{
			// Offline wins: presence is why nothing is routed at all.
			webhook: map[string]any{"state": "active", "presence": "offline", "host_presence": "online", "host_name": "hooks.example.com",
				"url_fingerprint": "sha256:abc", "keys_pending_channel_ids": []any{"btr_channel_e"}},
			want: []string{"Webhook offline: not delivering."},
		},
		{
			webhook: map[string]any{"state": "active", "presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc",
				"upgrade_required_member_ids": []any{"btr_member_m"}},
			want: []string{"State:    active\n", "Delivering.\nBlocked: member btr_member_m runs an agent without webhook support and must upgrade beam.\n"},
		},
	} {
		startWebhookDaemon(t, allWebhookCapabilities, func(string, string) (int, any) {
			return http.StatusOK, map[string]any{"webhook": sampleWebhook(test.webhook)}
		})
		stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "show", "btr_member_w")
		if code != ExitOK {
			t.Fatalf("code=%d stderr=%s", code, stderr)
		}
		for _, want := range test.want {
			if !strings.Contains(stdout, want) {
				t.Fatalf("show output lacks %q:\n%s", want, stdout)
			}
		}
		// A coordinator without durable delivery sends no durable setting.
		if _, sent := test.webhook["durable"]; !sent && strings.Contains(stdout, "Durable:") {
			t.Fatalf("show output names a durable setting the coordinator did not send:\n%s", stdout)
		}
	}
}

func TestRoomWebhookListingsShowStateAndReason(t *testing.T) {
	webhooks := []any{
		sampleWebhook(map[string]any{"state": "active", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"}),
		sampleWebhook(map[string]any{"member_id": "btr_member_v", "state": "degraded", "reason": "failing", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc",
			"local":                       map[string]any{"running": true, "health": map[string]any{"state": "degraded", "reason": "failing", "consecutive_failures": 6}, "circuit_open": true},
			"upgrade_required_member_ids": []any{"btr_member_m", "btr_member_n"}}),
		sampleWebhook(map[string]any{"member_id": "btr_member_u", "state": "active", "presence": "offline", "host_presence": "offline", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"}),
		sampleWebhook(map[string]any{"member_id": "btr_member_s", "state": "active", "presence": "offline", "host_presence": "online", "host_name": "hooks.example.com", "url_fingerprint": "sha256:abc"}),
		sampleWebhook(map[string]any{"member_id": "btr_member_t", "presence": "offline", "host_presence": "online"}),
		sampleWebhook(map[string]any{"member_id": "btr_member_r", "state": "active", "presence": "online", "host_presence": "online", "host_name": "hooks.example.com",
			"url_fingerprint": "sha256:abc", "keys_pending_channel_ids": []any{"btr_channel_e", "btr_channel_f"}}),
	}
	blocked := "Blocked webhook btr_member_v: members btr_member_m, btr_member_n run agents without webhook support and must upgrade beam.\n"
	startWebhookDaemon(t, allWebhookCapabilities, func(string, string) (int, any) {
		return http.StatusOK, map[string]any{"webhooks": webhooks}
	})
	stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "list")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	want := strings.Join([]string{
		"MEMBER ID     STATE     REASON   DELIVERY                                          HOST MEMBER   URL HOST           PAYLOAD   MAX FILE",
		"btr_member_w  active    -        delivering                                        btr_member_h  hooks.example.com  envelope  25 MiB",
		"btr_member_v  degraded  failing  delivering                                        btr_member_h  hooks.example.com  envelope  25 MiB",
		"btr_member_u  active    -        host offline: not delivering                      btr_member_h  hooks.example.com  envelope  25 MiB",
		"btr_member_s  active    -        webhook offline: not delivering                   btr_member_h  hooks.example.com  envelope  25 MiB",
		"btr_member_t  pending   -        not delivering                                    btr_member_h  -                  envelope  25 MiB",
		"btr_member_r  active    -        joining encryption: not delivering on 2 channels  btr_member_h  hooks.example.com  envelope  25 MiB",
		"",
	}, "\n") + blocked
	if stdout != want {
		t.Fatalf("list =\n%s\nwant\n%s", stdout, want)
	}
	stdout, stderr, code = runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "list", "--json")
	if code != ExitOK || !strings.Contains(stdout, `"upgrade_required_member_ids":["btr_member_m","btr_member_n"]`) || strings.Contains(stdout, "Blocked") {
		t.Fatalf("list --json must carry the members to upgrade unchanged: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"host_presence":"offline"`) || !strings.Contains(stdout, `"presence":"offline"`) || !strings.Contains(stdout, `"state":"active"`) {
		t.Fatalf("list --json lacks the presences: code=%d stderr=%s\n%s", code, stderr, stdout)
	}

	stdout, stderr, code = runWebhookCLI(newWebhookApp(), "webhook", "hosted")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"ROOM ID     MEMBER ID     STATE     REASON   URL HOST           DELIVERY", "running, 6 failed attempts in a row, one attempt at a time", blocked} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("hosted output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestRoomWebhookTestReportsAFailedDelivery(t *testing.T) {
	startWebhookDaemon(t, allWebhookCapabilities, func(string, string) (int, any) {
		return http.StatusOK, map[string]any{"test": map[string]any{"class": "auth", "http_status": 401, "attempts": 1, "reason": "http_401"}}
	})
	stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "test", "btr_member_w")
	if code != ExitOperationFailed {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "Test delivery: auth (HTTP 401, 1 attempt, http_401).") ||
		!strings.Contains(stderr, "beam room btr_room_a webhook rotate-secret btr_member_w") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestRoomMemberListShowsTheMemberKind(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tunnel.StatusPath {
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"memberships": []any{
			map[string]any{"member_id": "btr_member_a", "kind": "agent", "state": "active", "presence": "online", "agent_id": "agt_a"},
			map[string]any{"member_id": "btr_member_b", "kind": "object_storage", "state": "active", "presence": "online"},
			map[string]any{"member_id": "btr_member_w", "kind": "webhook", "state": "active", "presence": "offline", "agent_id": ""},
		}})
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "member", "list")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	want := strings.Join([]string{
		"MEMBER ID     KIND            STATE   PRESENCE  AGENT ID",
		"btr_member_a  agent           active  online    agt_a",
		"btr_member_b  object_storage  active  online    -",
		"btr_member_w  webhook         active  offline   -",
		"",
	}, "\n")
	if stdout != want {
		t.Fatalf("member list =\n%s\nwant\n%s", stdout, want)
	}
}

func TestRoomWebhookHelpShowsTheTrustModel(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{
		{"help", "room", "webhook"},
		{"room", "btr_room_a", "webhook", "accept", "--help"},
		{"room", "webhook", "--help"},
	} {
		stdout, stderr, code := runWebhookCLI(newWebhookApp(), args...)
		if code != ExitOK {
			t.Fatalf("%v: code=%d stderr=%s", args, code, stderr)
		}
		for _, want := range []string{"decrypts everything sent to the webhook", "receives live messages", "durable webhook", "--secret-file"} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("%v: help lacks %q:\n%s", args, want, stdout)
			}
		}
	}
	stdout, _, code := runWebhookCLI(newWebhookApp(), "help", "webhook")
	if code != ExitOK || !strings.Contains(stdout, "beam webhook hosted") {
		t.Fatalf("code=%d stdout=%s", code, stdout)
	}
}

// A development build names itself in every webhook hint, in results, progress
// lines and errors alike, so a copied command runs the same channel instead of
// the production CLI.
func TestRoomWebhookHintsNameTheRunningBinary(t *testing.T) {
	original := version.Version
	version.Version = "v0.0.0-dev.c85930cb2c64.7267ac677c0d"
	t.Cleanup(func() { version.Version = original })
	// A development version enables the update notice, which reads the CDN.
	t.Setenv("BEAM_NO_UPDATE_NOTICE", "1")
	productionCommand := regexp.MustCompile(`(^|[^\w-])beam (room|webhook|agent)\b`)
	secretFile := writeSecretFileForTest(t, testWebhookSecret, 0o600)
	webhook := func(overrides map[string]any) func(string, string) (int, any) {
		return func(string, string) (int, any) {
			return http.StatusOK, map[string]any{"webhook": sampleWebhook(overrides)}
		}
	}
	room := func(args ...string) []string { return append([]string{"room", "btr_room_a", "webhook"}, args...) }
	for _, test := range []struct {
		name    string
		args    []string
		respond func(string, string) (int, any)
		want    string
	}{
		{"add", room("add", "--name", "Alerts", "--host", "btr_member_h"), webhookResponses,
			"On the host's machine: beam-dev room btr_room_a webhook accept btr_member_w --url"},
		{"empty list", room("list"), func(string, string) (int, any) { return http.StatusOK, map[string]any{"webhooks": []any{}} },
			"`beam-dev room btr_room_a webhook add`"},
		{"show pending", room("show", "btr_member_w"), webhook(nil),
			"on its machine: beam-dev room btr_room_a webhook accept btr_member_w --url"},
		{"show without a host", room("show", "btr_member_w"), webhook(map[string]any{"host_member_id": ""}),
			"Name one with: beam-dev room btr_room_a webhook move btr_member_w --host"},
		{"show offline", room("show", "btr_member_w"), webhook(map[string]any{"state": "active", "presence": "offline", "host_presence": "online"}),
			"`beam-dev webhook hosted`"},
		{"show paused without a host", room("show", "btr_member_w"), webhook(map[string]any{"state": "paused", "reason": "host_left", "host_member_id": ""}),
			"Name a new host with: beam-dev room btr_room_a webhook move btr_member_w --host"},
		{"resume", room("resume", "btr_member_w"), webhook(map[string]any{"state": "paused", "reason": "gone"}),
			"Resume it with: beam-dev room btr_room_a webhook resume btr_member_w"},
		{"move", room("move", "btr_member_w", "--host", "btr_member_x"), webhookResponses,
			"On the new host's machine: beam-dev room btr_room_a webhook accept btr_member_w --url"},
		{"accept", room("accept", "btr_member_w", "--url", testWebhookURL, "--secret-file", secretFile, "--yes"), webhookResponses,
			"Send a signed test event with: beam-dev room btr_room_a webhook test btr_member_w"},
		{"hosted", []string{"webhook", "hosted"}, webhookResponses,
			"Accept a pending webhook with: beam-dev room <room-id> webhook accept <member-id>"},
		{"error", room("resume", "btr_member_w"), func(string, string) (int, any) {
			return http.StatusConflict, map[string]any{"error": map[string]any{"code": "conflict", "message": "btr webhook state does not allow this change"}}
		}, "`beam-dev room btr_room_a webhook move btr_member_w --host <member-id>`"},
	} {
		t.Run(test.name, func(t *testing.T) {
			startWebhookDaemon(t, allWebhookCapabilities, test.respond)
			stdout, stderr, _ := runWebhookCLI(newWebhookApp(), test.args...)
			output := stdout + stderr
			if !strings.Contains(output, test.want) {
				t.Fatalf("output lacks %q:\nstdout=%s\nstderr=%s", test.want, stdout, stderr)
			}
			if productionCommand.MatchString(output) {
				t.Fatalf("output names the production binary:\nstdout=%s\nstderr=%s", stdout, stderr)
			}
		})
	}
}

var durableWebhookCapabilities = []string{webhookManageCapability, webhookHostCapability, webhookDurableCapability}

// durableResponses answers the durable routes; the webhook delivers durably
// on one channel.
func durableResponses(method, path string) (int, any) {
	local := map[string]any{"running": true, "durable": true, "durable_channel_ids": []any{"btr_chan_1"},
		"spool": map[string]any{"pending": 3, "dead_letters": 1, "bytes": 2048, "share_bytes": 1 << 30}}
	switch {
	case strings.HasSuffix(path, "/dead-letters"):
		return http.StatusOK, map[string]any{"dead_letters": []any{map[string]any{"publication_id": "btr_pub_9",
			"channel_id": "btr_chan_1", "event": "message", "received_at": "2026-10-09T12:00:00Z", "attempts": 4,
			"http_status": 404, "reason": "http_404", "keep_until": "2026-10-17T12:00:00Z"}}}
	case strings.HasSuffix(path, "/replay"):
		return http.StatusOK, map[string]any{"replayed": 1}
	case strings.HasSuffix(path, "/purge"):
		return http.StatusOK, map[string]any{"purged": 2}
	case method == http.MethodPatch:
		return http.StatusOK, map[string]any{"webhook": sampleWebhook(map[string]any{"state": "active", "local": local})}
	case path == "/v1/hosted-webhooks":
		return http.StatusOK, map[string]any{"webhooks": []any{sampleWebhook(map[string]any{"state": "active",
			"host_name": "hooks.example.com", "local": local})}}
	default:
		return webhookResponses(method, path)
	}
}

func TestRoomWebhookDurableDelivery(t *testing.T) {
	for _, test := range []struct {
		args []string
		body map[string]any
		want string
	}{
		{[]string{"durable", "btr_member_w", "on"}, map[string]any{"durable": true},
			"now delivers durably. Events of 1 channel are kept on this machine, encrypted, and retried for up to 24 hours."},
		{[]string{"durable", "btr_member_w", "off"}, map[string]any{"durable": false}, "now delivers live only"},
	} {
		daemon := startWebhookDaemon(t, durableWebhookCapabilities, durableResponses)
		stdout, stderr, code := runWebhookCLI(newWebhookApp(), append([]string{"room", "btr_room_a", "webhook"}, test.args...)...)
		calls := daemon.routeCalls()
		if code != ExitOK || len(calls) != 1 || calls[0].Method != http.MethodPatch ||
			calls[0].Path != "/v1/hosted-webhooks/btr_room_a/btr_member_w" || !reflect.DeepEqual(calls[0].Body, test.body) {
			t.Fatalf("%v: code=%d stderr=%s calls=%+v", test.args, code, stderr, calls)
		}
		if !strings.Contains(stdout, test.want) {
			t.Fatalf("%v printed %q", test.args, stdout)
		}
	}

	daemon := startWebhookDaemon(t, durableWebhookCapabilities, durableResponses)
	stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "accept", "btr_member_w",
		"--url", testWebhookURL, "--secret-file", writeSecretFileForTest(t, testWebhookSecret, 0o600), "--durable", "--yes")
	if calls := daemon.routeCalls(); code != ExitOK || len(calls) != 1 || calls[0].Body["durable"] != true {
		t.Fatalf("accept --durable: code=%d stderr=%s calls=%+v", code, stderr, calls)
	}
	// The accept response has no running webhook yet, so its channels are
	// not known.
	if !strings.Contains(stdout, "Once the webhook runs, `beam webhook hosted` shows the channels") ||
		strings.Contains(stdout, "No channel") {
		t.Fatalf("accept --durable printed %q", stdout)
	}

	stdout, stderr, code = runWebhookCLI(newWebhookApp(), "webhook", "hosted")
	if code != ExitOK || !strings.Contains(stdout, "running, durable on 1 channel: 3 pending, 1 dead letter, 2.0 KiB of 1.0 GiB") {
		t.Fatalf("hosted: code=%d stderr=%s\n%s", code, stderr, stdout)
	}

	startWebhookDaemon(t, allWebhookCapabilities, durableResponses)
	if _, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "durable", "btr_member_w", "on"); code == ExitOK {
		t.Fatalf("durable without the agent capability succeeded: %s", stderr)
	}
}

func TestRoomWebhookDeadLetters(t *testing.T) {
	daemon := startWebhookDaemon(t, durableWebhookCapabilities, durableResponses)
	stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "dead-letters", "list", "btr_member_w")
	if code != ExitOK || !strings.Contains(stdout, "btr_pub_9") || !strings.Contains(stdout, "http_404") {
		t.Fatalf("list: code=%d stderr=%s\n%s", code, stderr, stdout)
	}
	stdout, _, code = runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "dead-letters", "replay", "btr_member_w", "btr_pub_9")
	if code != ExitOK || !strings.Contains(stdout, "1 dead letter of webhook btr_member_w replayed; it is delivered again with its original Idempotency-Key.") {
		t.Fatalf("replay printed %q", stdout)
	}
	stdout, _, code = runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "dead-letters", "purge", "btr_member_w", "--all")
	if code != ExitOK || !strings.Contains(stdout, "2 dead letters of webhook btr_member_w deleted") {
		t.Fatalf("purge printed %q", stdout)
	}
	calls := daemon.routeCalls()
	want := []webhookCall{
		{Method: http.MethodGet, Path: "/v1/hosted-webhooks/btr_room_a/btr_member_w/dead-letters"},
		{Method: http.MethodPost, Path: "/v1/hosted-webhooks/btr_room_a/btr_member_w/dead-letters/replay",
			Body: map[string]any{"publication_ids": []any{"btr_pub_9"}}},
		{Method: http.MethodPost, Path: "/v1/hosted-webhooks/btr_room_a/btr_member_w/dead-letters/purge",
			Body: map[string]any{"all": true}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %+v\nwant %+v", calls, want)
	}
	for _, args := range [][]string{
		{"dead-letters"},
		{"dead-letters", "show", "btr_member_w"},
		{"dead-letters", "replay", "btr_member_w"},
		{"dead-letters", "purge", "btr_member_w", "btr_pub_9", "--all"},
		{"dead-letters", "list", "btr_member_w", "btr_pub_9"},
		{"durable", "btr_member_w"},
		{"durable", "btr_member_w", "maybe"},
	} {
		if _, _, code := runWebhookCLI(newWebhookApp(), append([]string{"room", "btr_room_a", "webhook"}, args...)...); code != ExitUsage {
			t.Fatalf("%v: code=%d, want usage", args, code)
		}
	}
}

func TestHostedDeliveryNamesTheDurableChannels(t *testing.T) {
	for _, test := range []struct {
		local webhookLocalView
		want  string
	}{
		{webhookLocalView{Running: true, Durable: true}, "running, durable but no channel allows receiver_local"},
		{webhookLocalView{Durable: true}, "stopped, durable"},
		{webhookLocalView{Running: true, Durable: true, DurableChannelIDs: []string{"btr_chan_1", "btr_chan_2"},
			Spool: &webhookSpoolView{Pending: 1}}, "running, durable on 2 channels: 1 pending, 0 dead letters, 0 bytes of 0 bytes"},
		{webhookLocalView{Running: true, Spool: &webhookSpoolView{Pending: 2}}, "running, 2 kept events still pending"},
	} {
		if got := hostedDelivery(&test.local); got != test.want {
			t.Fatalf("hostedDelivery(%+v) = %q, want %q", test.local, got, test.want)
		}
	}
}

var asyncWebhookCapabilities = []string{webhookManageCapability, webhookHostCapability, webhookAsyncCapability}

func TestRoomWebhookAsyncReplies(t *testing.T) {
	daemon := startWebhookDaemon(t, asyncWebhookCapabilities, durableResponses)
	stdout, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "reply-window", "btr_member_w", "2h")
	calls := daemon.routeCalls()
	if code != ExitOK || len(calls) != 1 || calls[0].Method != http.MethodPatch ||
		calls[0].Path != "/v1/hosted-webhooks/btr_room_a/btr_member_w" ||
		!reflect.DeepEqual(calls[0].Body, map[string]any{"async_reply_window_seconds": float64(7200)}) {
		t.Fatalf("reply-window: code=%d stderr=%s calls=%+v", code, stderr, calls)
	}
	if !strings.Contains(stdout, "polls for an asynchronous reply for up to 2h0m0s") {
		t.Fatalf("reply-window printed %q", stdout)
	}
	for _, value := range []string{"30s", "25h", "1.5s", "soon"} {
		if _, _, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "reply-window", "btr_member_w", value); code != ExitUsage {
			t.Fatalf("reply-window %s: code=%d", value, code)
		}
	}

	daemon = startWebhookDaemon(t, asyncWebhookCapabilities, durableResponses)
	_, stderr, code = runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "accept", "btr_member_w",
		"--url", testWebhookURL, "--secret-file", writeSecretFileForTest(t, testWebhookSecret, 0o600), "--reply-window", "90m", "--yes")
	if calls := daemon.routeCalls(); code != ExitOK || len(calls) != 1 || calls[0].Body["async_reply_window_seconds"] != float64(5400) {
		t.Fatalf("accept --reply-window: code=%d stderr=%s calls=%+v", code, stderr, calls)
	}

	startWebhookDaemon(t, allWebhookCapabilities, durableResponses)
	if _, stderr, code := runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "reply-window", "btr_member_w", "2h"); code == ExitOK {
		t.Fatalf("reply-window without the agent capability succeeded: %s", stderr)
	}

	startWebhookDaemon(t, allWebhookCapabilities, func(string, string) (int, any) {
		return http.StatusOK, map[string]any{"test": map[string]any{"class": "delivered", "http_status": 202, "attempts": 1,
			"async_reply": true}}
	})
	stdout, stderr, code = runWebhookCLI(newWebhookApp(), "room", "btr_room_a", "webhook", "test", "btr_member_w")
	if code != ExitOK || !strings.Contains(stdout, "The response starts an asynchronous reply") {
		t.Fatalf("test: code=%d stderr=%s stdout=%q", code, stderr, stdout)
	}

	startWebhookDaemon(t, allWebhookCapabilities, func(string, string) (int, any) {
		return http.StatusOK, map[string]any{"webhooks": []any{sampleWebhook(map[string]any{"state": "active",
			"host_name": "hooks.example.com", "local": map[string]any{"running": true, "async_replies": 2}})}}
	})
	stdout, stderr, code = runWebhookCLI(newWebhookApp(), "webhook", "hosted")
	if code != ExitOK || !strings.Contains(stdout, "running, 2 replies polled for") {
		t.Fatalf("hosted: code=%d stderr=%s\n%s", code, stderr, stdout)
	}
}
