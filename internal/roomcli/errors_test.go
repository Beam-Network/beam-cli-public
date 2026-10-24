//go:build !windows

package roomcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

// failingRunner answers every daemon request with status and body.
func failingRunner(t *testing.T, status int, body any) (Runner, *bytes.Buffer) {
	t.Helper()
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	var stderr bytes.Buffer
	return Runner{Client: tunnel.NewLocalClient(socket), Out: &bytes.Buffer{}, Err: &stderr}, &stderr
}

func TestExecuteExplainsKnownFailures(t *testing.T) {
	t.Setenv("BEAM_API_KEY", "b1m_test")
	for _, test := range []struct {
		name        string
		args        []string
		status      int
		body        any
		wantCode    int
		wantKind    string
		wantMessage string
		wantHint    string
	}{
		{
			name: "create from an invitation-only agent", args: []string{"create"}, status: http.StatusForbidden,
			body:     map[string]any{"error": map[string]any{"code": "permission_denied", "message": "creating a room requires an agent connected through a Beam account"}},
			wantCode: ExitDenied, wantKind: "room_create_requires_account_agent",
			wantMessage: "joined through a Room invitation", wantHint: "beam agent connect",
		},
		{
			name: "create with an expired key", args: []string{"create"}, status: http.StatusUnauthorized,
			body:     map[string]any{"error": map[string]any{"code": "api_key_invalid", "message": "API key is expired"}},
			wantCode: ExitDenied, wantKind: "room_api_key_invalid",
			wantMessage: "unknown, expired, revoked or disabled", wantHint: "Renew or replace the key",
		},
		{
			name: "create with a disabled key", args: []string{"create"}, status: http.StatusForbidden,
			body:     map[string]any{"error": map[string]any{"code": "api_key_invalid", "message": "API key is disabled"}},
			wantCode: ExitDenied, wantKind: "room_api_key_invalid",
			wantMessage: "refused the API key", wantHint: "Beam Console",
		},
		{
			name: "create without credit", args: []string{"create"}, status: http.StatusPaymentRequired,
			body: map[string]any{"error": map[string]any{
				"code": "insufficient_credits", "message": "Organization has no credits remaining",
				"details": map[string]any{"credits_required": 1, "credits_available": 0.25, "reason": "pool_exhausted"},
			}},
			wantCode: ExitFailed, wantKind: "room_insufficient_credits",
			wantMessage: "no credits remaining (credits required: 1, available: 0.25; reason: pool_exhausted)", wantHint: "Add credits",
		},
		{
			name: "create over the key's credit limit", args: []string{"create"}, status: http.StatusPaymentRequired,
			body: map[string]any{"error": map[string]any{
				"code": "insufficient_credits", "message": "The API key cannot pay for this room.",
				"credits_required": 1, "credits_available": 0, "reason": "key_cap_exhausted",
			}},
			wantCode: ExitFailed, wantKind: "room_insufficient_credits",
			wantMessage: "(credits required: 1, available: 0; reason: key_cap_exhausted)", wantHint: "credit limit",
		},
		{
			name: "create with a key lacking rooms:start", args: []string{"create"}, status: http.StatusForbidden,
			body: map[string]any{"error": map[string]any{
				"code": "permission_required", "message": "API key is not allowed to run billable actions",
				"details": map[string]any{"permission": "rooms:start"},
			}},
			wantCode: ExitDenied, wantKind: "room_permission_required",
			wantMessage: "billable actions (permission: rooms:start)", wantHint: "role grants the permission",
		},
		{
			name: "create for a blocked organization", args: []string{"create"}, status: http.StatusForbidden,
			body:     map[string]any{"error": map[string]any{"code": "organization_blocked", "message": "This organization cannot start rooms."}},
			wantCode: ExitDenied, wantKind: "room_organization_blocked",
			wantMessage: "cannot start rooms", wantHint: "Beam support",
		},
		{
			name: "legacy string error code", args: []string{"create"}, status: http.StatusPaymentRequired,
			body:     map[string]any{"error": "insufficient_credits", "message": "Organization has no credits remaining"},
			wantCode: ExitFailed, wantKind: "room_insufficient_credits",
			wantMessage: "no credits remaining", wantHint: "Add credits",
		},
		{
			name: "unknown failure keeps the daemon message", args: []string{"inspect", "btr_room_a"}, status: http.StatusInternalServerError,
			body:     map[string]any{"error": map[string]any{"code": "internal", "message": "boom"}},
			wantCode: ExitFailed, wantMessage: "boom",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := failingRunner(t, test.status, test.body)
			failure := runner.Execute(context.Background(), append([]string{"tunnel", "room"}, test.args...))
			if failure == nil {
				t.Fatal("command succeeded")
			}
			if failure.Code != test.wantCode || failure.Kind != test.wantKind {
				t.Fatalf("failure=%+v, want code %d kind %q", failure, test.wantCode, test.wantKind)
			}
			if !strings.Contains(strings.ToLower(failure.Message), strings.ToLower(test.wantMessage)) {
				t.Fatalf("message=%q, want %q", failure.Message, test.wantMessage)
			}
			if test.wantHint == "" && failure.Hint != "" || !strings.Contains(failure.Hint, test.wantHint) {
				t.Fatalf("hint=%q, want %q", failure.Hint, test.wantHint)
			}
		})
	}
}

func TestRunPrintsHintAfterMessage(t *testing.T) {
	t.Setenv("BEAM_API_KEY", "b1m_test")
	runner, stderr := failingRunner(t, http.StatusForbidden, map[string]any{
		"error": map[string]any{"code": "permission_denied", "message": "creating a room requires an agent connected through a Beam account"},
	})
	if code := runner.Run(context.Background(), []string{"tunnel", "room", "create"}); code != ExitDenied {
		t.Fatalf("code=%d", code)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "beam room: ") || !strings.HasPrefix(lines[1], "  hint: ") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestErrorRuleMatchesTypedCodeBeforePhrase(t *testing.T) {
	rule := errorRule{Codes: []string{"new_typed_code"}, Commands: []string{"channel publish"}}
	if !rule.matches("channel publish", &tunnel.Error{Code: "NEW_TYPED_CODE", Detail: "anything"}) {
		t.Fatal("typed code did not match case-insensitively")
	}
	if rule.matches("channel listen", &tunnel.Error{Code: "new_typed_code"}) {
		t.Fatal("rule matched a command it is not scoped to")
	}
	if rule.matches("channel publish", &tunnel.Error{Detail: "new_typed_code"}) {
		t.Fatal("a code must not match through the message text")
	}
}

func TestErrorRuleMatchesEveryTypedReason(t *testing.T) {
	rule := errorRule{TypedReasons: []string{"room_workload_relay_unavailable"}, Commands: []string{"channel datagram"}}
	for _, err := range []*tunnel.Error{
		{Code: "datagram_failed", Details: map[string]any{"reason": "room_workload_relay_unavailable"}},
		{Code: "datagram_failed", Details: map[string]any{"reasons": []any{"worker_unavailable", "ROOM_WORKLOAD_RELAY_UNAVAILABLE"}}},
		{Code: "datagram_failed", Fields: map[string]any{"reasons": []any{"room_workload_relay_unavailable"}}},
	} {
		if !rule.matches("channel datagram", err) {
			t.Fatalf("typed reason not matched in %+v", err)
		}
	}
	for _, err := range []*tunnel.Error{
		{Code: "datagram_failed", Details: map[string]any{"reasons": []any{"worker_unavailable"}}},
		{Code: "datagram_failed", Detail: "room_workload_relay_unavailable"},
	} {
		if rule.matches("channel datagram", err) {
			t.Fatalf("matched without the typed reason: %+v", err)
		}
	}
}

func TestExplainWebhookFailures(t *testing.T) {
	for _, test := range []struct {
		command  string
		err      *tunnel.Error
		wantKind string
		wantCode int
	}{
		{command: "webhook add", err: &tunnel.Error{Status: 403, Code: "permission_denied", Detail: "BTR authorization denied"}, wantKind: "webhook_permission_denied", wantCode: ExitDenied},
		{command: "webhook accept", err: &tunnel.Error{Status: 403, Code: "permission_denied", Detail: "BTR authorization denied"}, wantKind: "webhook_host_permission_denied", wantCode: ExitDenied},
		{command: "webhook move", err: &tunnel.Error{Status: 422, Code: "unsupported_capability", Detail: "btr webhook host is not eligible"}, wantKind: "webhook_host_ineligible", wantCode: ExitFailed},
		{command: "webhook resume", err: &tunnel.Error{Kind: tunnel.ErrConflict, Status: 409, Code: "conflict", Detail: "btr webhook state does not allow this change"}, wantKind: "webhook_state_conflict", wantCode: ExitConflict},
		{command: "webhook add", err: &tunnel.Error{Status: 429, Code: "resource_exhausted", Detail: "BTR control quota exceeded"}, wantKind: "webhook_limit_reached", wantCode: ExitFailed},
		{command: "webhook test", err: &tunnel.Error{Kind: tunnel.ErrNotFound, Status: 404, Code: "not_found", Detail: "BTR resource not found"}, wantKind: "webhook_not_hosted", wantCode: ExitNotFound},
		{command: "webhook show", err: &tunnel.Error{Kind: tunnel.ErrNotFound, Status: 404, Code: "not_found", Detail: "BTR resource not found"}, wantKind: "webhook_not_found", wantCode: ExitNotFound},
	} {
		failure, ok := Explain(test.command, test.err)
		if !ok || failure.Kind != test.wantKind || failure.Code != test.wantCode || failure.Message == "" || failure.Hint == "" {
			t.Errorf("%s %+v: explained=%v failure=%+v", test.command, test.err, ok, failure)
		}
	}
	retention := &tunnel.Error{Status: 422, Code: "webhook_channel_requires_retention", Detail: "channel btr_chan_1 requires retention"}
	if failure, ok := Explain("webhook add", retention); !ok || failure.Message != retention.Detail {
		t.Errorf("a rule without a message must keep the daemon's: %+v", failure)
	}
	upgrade := &tunnel.Error{Kind: tunnel.ErrConflict, Status: 409, Code: "webhook_controller_upgrade_required",
		Detail: "member(s) btr_member_m may control a channel this webhook uses and run an agent without webhook member support: they must upgrade beam"}
	for _, command := range []string{"webhook add", "webhook accept", "channel grant put"} {
		failure, ok := Explain(command, upgrade)
		if !ok || failure.Kind != "webhook_controller_upgrade_required" || failure.Code != ExitConflict ||
			failure.Message != upgrade.Detail || !strings.Contains(failure.Hint, "beam update") {
			t.Errorf("%s: upgrade refusal must name the members and say how to unblock: %+v", command, failure)
		}
	}
	if _, ok := Explain("webhook add", &tunnel.Error{Status: 500, Code: "internal", Detail: "boom"}); ok {
		t.Error("an unknown failure was explained")
	}
	if failure := MissingWebhookCapability(true); failure.Kind != "webhook_host_unavailable" || failure.Code != ExitFailed {
		t.Errorf("host capability failure = %+v", failure)
	}
	if failure := MissingWebhookCapability(false); failure.Kind != "webhook_manage_unavailable" {
		t.Errorf("manage capability failure = %+v", failure)
	}
}

func TestRoomCommandName(t *testing.T) {
	for args, want := range map[string]string{
		"tunnel room create":                        "create",
		"tunnel room channel publish r c --literal": "channel publish",
		"tunnel room grant put r c":                 "grant put",
		"tunnel room inspect r":                     "inspect",
	} {
		if got := roomCommandName(strings.Fields(args)); got != want {
			t.Fatalf("roomCommandName(%q)=%q want %q", args, got, want)
		}
	}
}

func publishRunner(t *testing.T) (Runner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/publish") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"publication_id": "btr_pub_1"})
	}))
	var stdout, stderr bytes.Buffer
	return Runner{Client: tunnel.NewLocalClient(socket), Out: &stdout, Err: &stderr}, &stdout, &stderr
}

func TestPublishWithoutRetentionExplainsLiveOnlyDelivery(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		notice bool
	}{
		{name: "default retention", args: []string{"--literal", "hi"}, notice: true},
		{name: "explicit none", args: []string{"--literal", "hi", "--retention", "none"}, notice: true},
		{name: "sender local", args: []string{"--literal", "hi", "--retention", "sender_local"}},
		{name: "json stays machine readable", args: []string{"--literal", "hi", "--json"}},
		{name: "quiet stays silent", args: []string{"--literal", "hi", "--quiet"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, stdout, stderr := publishRunner(t)
			args := append([]string{"tunnel", "room", "channel", "publish", "btr_room_a", "btr_channel_a"}, test.args...)
			if code := runner.Run(context.Background(), args); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if got := strings.Contains(stderr.String(), "live only"); got != test.notice {
				t.Fatalf("notice=%v want %v stderr=%q", got, test.notice, stderr.String())
			}
			if strings.Contains(stdout.String(), "live only") {
				t.Fatalf("notice leaked into stdout: %q", stdout.String())
			}
		})
	}
}

func TestNoticesCanBypassBufferedErrorStream(t *testing.T) {
	runner, _, stderr := publishRunner(t)
	var notices bytes.Buffer
	runner.Notices = &notices
	if code := runner.Run(context.Background(), []string{"tunnel", "room", "channel", "publish", "r", "c", "--literal", "hi"}); code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if stderr.Len() != 0 || !strings.Contains(notices.String(), "live only") {
		t.Fatalf("stderr=%q notices=%q", stderr.String(), notices.String())
	}
}

func TestLiveOnlyPublishNoticeReadsDeliveryCounts(t *testing.T) {
	for _, test := range []struct {
		name     string
		response map[string]any
		want     string
	}{
		{name: "older daemon without counts", response: map[string]any{"publication_id": "p"}, want: "delivered live only"},
		{name: "nobody listening", response: map[string]any{"online_deliveries": float64(0)}, want: "nobody received this message"},
		{name: "targeted but nobody listening", response: map[string]any{"online_deliveries": float64(2), "delivered_deliveries": float64(0), "failed_deliveries": float64(0), "skipped_online_only": float64(2)}, want: "nobody received this message"},
		{name: "some members missed it", response: map[string]any{"online_deliveries": float64(3), "delivered_deliveries": float64(1), "failed_deliveries": float64(0), "skipped_online_only": float64(2)}, want: "2 members were not listening"},
		{name: "older daemon without a failed count", response: map[string]any{"online_deliveries": float64(3), "delivered_deliveries": float64(1)}, want: "delivered to 1 member; 2 members were not listening"},
		{name: "one member skipped", response: map[string]any{"online_deliveries": float64(2), "delivered_deliveries": float64(1), "skipped_online_only": float64(1)}, want: "1 member was not listening"},
		{name: "newer agent reports a not-listening count", response: map[string]any{"online_deliveries": float64(2), "delivered_deliveries": float64(1), "not_listening": float64(1)}, want: "delivered to 1 member; 1 member was not listening"},
		{name: "online count without delivered", response: map[string]any{"online_deliveries": float64(3), "skipped_online_only": float64(1)}, want: "delivered to 2 members; 1 member was not listening"},
		{name: "everyone listening", response: map[string]any{"online_deliveries": float64(3), "delivered_deliveries": float64(3), "failed_deliveries": float64(0)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := liveOnlyPublishNotice(test.response)
			if test.want == "" {
				if got != "" {
					t.Fatalf("notice=%q, want none", got)
				}
				return
			}
			if !strings.Contains(got, test.want) || !strings.Contains(got, "hint: start `listen`") {
				t.Fatalf("notice=%q, want %q", got, test.want)
			}
		})
	}
}

// A delivery that failed for a listening member is not a member that was not
// listening (D10): an agent can report it with a successful publish when some
// other member received the message.
func TestLiveOnlyPublishNoticeReportsFailedDeliveries(t *testing.T) {
	for _, test := range []struct {
		name     string
		response map[string]any
		want     []string
		avoid    []string
	}{
		{
			name: "delivered and failed",
			response: map[string]any{"online_deliveries": float64(2), "delivered_deliveries": float64(1), "failed_deliveries": float64(1), "skipped_online_only": float64(0),
				"failed_member_ids": []any{"btr_member_c"}, "failure_reason": "MLS decryption failed"},
			want:  []string{"beam room: delivery failed for 1 listening member (btr_member_c): MLS decryption failed. It was delivered to 1 member.", "hint: the failure may be temporary", "get it again", "`beam agent logs`"},
			avoid: []string{"not listening", "nobody received"},
		},
		{
			name:     "delivered, failed, and not listening",
			response: map[string]any{"online_deliveries": float64(4), "delivered_deliveries": float64(2), "failed_deliveries": float64(1), "skipped_online_only": float64(1)},
			want:     []string{"delivery failed for 1 listening member. It was delivered to 2 members; 1 member was not listening", "`beam agent logs`", "need `listen` running"},
		},
		{
			// A re-key overtook the message after it reached some
			// members; the agent names who received it and who did not.
			name: "partly delivered during a re-key",
			response: map[string]any{"online_deliveries": float64(4), "delivered_deliveries": float64(2), "failed_deliveries": float64(1),
				"expired_deliveries": float64(1), "skipped_online_only": float64(0),
				"delivered_member_ids": []any{"btr_member_b", "btr_member_c"},
				"member_failures": []any{
					map[string]any{"member_id": "btr_member_d", "reason": "stale_scope"},
					map[string]any{"member_id": "btr_member_e", "reason": "delivery_unconfirmed"}}},
			want: []string{"delivery failed for 1 listening member (btr_member_d): the channel was re-keyed while the message was in flight. " +
				"1 member (btr_member_e) did not confirm it, so they may have it. It was delivered to 2 members (btr_member_b, btr_member_c).",
				"get it again"},
			avoid: []string{"not listening", "nobody received", "nothing was sent"},
		},
		{
			name: "delivered with members that never answered",
			response: map[string]any{"online_deliveries": float64(3), "delivered_deliveries": float64(1), "failed_deliveries": float64(0),
				"expired_deliveries": float64(2), "skipped_online_only": float64(0), "delivered_member_ids": []any{"btr_member_b"},
				"member_failures": []any{
					map[string]any{"member_id": "btr_member_c", "reason": "delivery_unconfirmed"},
					map[string]any{"member_id": "btr_member_d", "reason": "delivery_unconfirmed"}}},
			want: []string{"beam room: 2 members (btr_member_c, btr_member_d) did not confirm it, so they may have it. It was delivered to 1 member (btr_member_b).",
				"hint: their agents did not answer in time", "can deliver the message twice"},
			avoid: []string{"not listening", "nobody received", "delivery failed"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := liveOnlyPublishNotice(test.response)
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("notice=%q, want %q", got, want)
				}
			}
			for _, avoid := range test.avoid {
				if strings.Contains(got, avoid) {
					t.Fatalf("notice=%q must not contain %q", got, avoid)
				}
			}
		})
	}
}

func TestPublishNoticeWithFailedDeliveryStaysOffJSONAndQuiet(t *testing.T) {
	for _, test := range []struct {
		flag   string
		notice bool
	}{{notice: true}, {flag: "--json"}, {flag: "--quiet"}} {
		t.Run("flag "+test.flag, func(t *testing.T) {
			runner, _ := failingRunner(t, http.StatusOK, map[string]any{"publication_id": "btr_pub_1",
				"online_deliveries": 2, "delivered_deliveries": 1, "failed_deliveries": 1, "skipped_online_only": 0})
			var notices bytes.Buffer
			runner.Notices = &notices
			args := []string{"tunnel", "room", "channel", "publish", "btr_room_a", "btr_channel_a", "--literal", "hi"}
			if test.flag != "" {
				args = append(args, test.flag)
			}
			if code := runner.Run(context.Background(), args); code != ExitOK {
				t.Fatalf("code=%d", code)
			}
			if got := strings.Contains(notices.String(), "delivery failed for 1 listening member"); got != test.notice {
				t.Fatalf("notice=%v want %v (%q)", got, test.notice, notices.String())
			}
		})
	}
}

// fakeChannel describes how a fake daemon answers the requests a failed
// channel command and its diagnosis make.
type fakeChannel struct {
	status int // publish/listen status; http.StatusOK on listen is an empty stream
	body   any
	// channel answers GET .../channels/{id}; nil means visible and plain.
	channel map[string]any
	// hidden makes GET .../channels/{id} answer the coordinator's not_found.
	hidden bool
	// provisioning answers GET .../provisioning; nil means not found.
	provisioning map[string]any
	// workload answers GET .../workloads/{id}; nil means not found.
	workload map[string]any
}

func notFound(w http.ResponseWriter, code, message string) {
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func channelRunner(t *testing.T, fake fakeChannel) Runner {
	t.Helper()
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch path := r.URL.Path; {
		case strings.HasSuffix(path, "/provisioning"):
			if fake.provisioning == nil {
				notFound(w, "provisioning_unknown", "this agent has not run channel provisioning for that channel.")
				return
			}
			_ = json.NewEncoder(w).Encode(fake.provisioning)
		case strings.Contains(path, "/workloads/"):
			if fake.workload == nil {
				notFound(w, "not_found", "BTR resource not found")
				return
			}
			_ = json.NewEncoder(w).Encode(fake.workload)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/channels/btr_channel_a"):
			if fake.hidden {
				notFound(w, "not_found", "BTR resource not found")
				return
			}
			channel := fake.channel
			if channel == nil {
				channel = map[string]any{"channel": map[string]any{"channel_id": "btr_channel_a", "state": "active"}}
			}
			_ = json.NewEncoder(w).Encode(channel)
		case strings.HasSuffix(path, "/listen") && fake.status == http.StatusOK:
			w.WriteHeader(http.StatusOK) // an empty NDJSON stream
		default:
			w.WriteHeader(fake.status)
			_ = json.NewEncoder(w).Encode(fake.body)
		}
	}))
	return Runner{Client: tunnel.NewLocalClient(socket), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
}

func daemonError(code, message string, extra map[string]any) map[string]any {
	object := map[string]any{"code": code, "message": message}
	for key, value := range extra {
		object[key] = value
	}
	return map[string]any{"error": object}
}

const (
	noManagerText       = "no eligible MLS delivery controller: no member agent has Manage on channel btr_channel_a; grant Manage on the channel to a member agent"
	managersOfflineText = "no eligible MLS delivery controller: no online member with Manage on channel btr_channel_a; 2 member agent(s) with Manage are offline"
)

func controllerUnavailable(reason, text string) map[string]any {
	return map[string]any{"code": "mls_delivery_controller_unavailable", "reason": reason, "channel_id": "btr_channel_a", "error": text}
}

type channelCase struct {
	name        string
	args        []string
	fake        fakeChannel
	wantKind    string
	wantMessage string
	wantHint    string
	// avoid lists text neither the message nor the hint may contain.
	avoid []string
}

var (
	publishArgs    = []string{"channel", "publish", "btr_room_a", "btr_channel_a", "--literal", "hi"}
	listenArgs     = []string{"channel", "listen", "btr_room_a", "btr_channel_a"}
	mlsUnavailable = daemonError("publish_failed", "MLS channel state is unavailable", nil)
)

func runChannelCases(t *testing.T, cases []channelCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := channelRunner(t, test.fake)
			failure := runner.Execute(context.Background(), append([]string{"tunnel", "room"}, test.args...))
			if failure == nil {
				t.Fatal("command succeeded")
			}
			if failure.Kind != test.wantKind {
				t.Fatalf("kind=%q want %q (failure=%+v)", failure.Kind, test.wantKind, failure)
			}
			if !strings.Contains(failure.Message, test.wantMessage) || !strings.Contains(failure.Hint, test.wantHint) {
				t.Fatalf("failure=%+v, want message %q and hint %q", failure, test.wantMessage, test.wantHint)
			}
			for _, avoid := range test.avoid {
				if strings.Contains(failure.Message+failure.Hint, avoid) {
					t.Fatalf("failure=%+v must not contain %q", failure, avoid)
				}
			}
			// A plain member cannot run grant commands, so a hint may only
			// suggest one while saying who can.
			if strings.Contains(failure.Hint, "grant put") && !strings.Contains(failure.Hint, "Only a Room owner or a member holding manage") {
				t.Fatalf("hint suggests grant put without saying who can run it: %q", failure.Hint)
			}
			if strings.Contains(failure.Hint, "grant list`") {
				t.Fatalf("hint suggests grant list, which a plain member cannot run: %q", failure.Hint)
			}
			if strings.Contains(failure.Hint, "ROOM_ID") || strings.Contains(failure.Hint, "CHANNEL_ID") || strings.Contains(failure.Hint, "ACTION") {
				t.Fatalf("hint kept a placeholder: %q", failure.Hint)
			}
		})
	}
}

func TestExecuteExplainsChannelFailures(t *testing.T) {
	runChannelCases(t, []channelCase{
		{
			name: "channel not ready without provisioning state", args: publishArgs, fake: fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable},
			wantKind: "channel_not_ready", wantMessage: "encryption is not ready", wantHint: "Likely causes",
		},
		{
			// The phase can lag behind a join that already happened (D5), so
			// it only yields likely causes, never a claim that setup is stuck.
			name: "bootstrapping phase lists likely causes", args: publishArgs,
			fake:     fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable, provisioning: map[string]any{"phase": "bootstrapping", "attempts": 12, "since": "2026-09-27T10:00:00Z"}},
			wantKind: "channel_not_ready", wantMessage: "encryption is not ready", wantHint: `phase "bootstrapping" (for `,
			avoid: []string{"has been in phase", "stuck"},
		},
		{
			name: "active channel means the grant is missing", args: publishArgs,
			fake:     fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable, provisioning: map[string]any{"phase": "active"}},
			wantKind: "channel_permission_denied", wantMessage: "missing the publish grant", wantHint: "--actions publish`",
		},
		{
			name: "provisioning last_error names the missing manager", args: publishArgs,
			fake:     fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable, provisioning: map[string]any{"phase": "bootstrapping", "last_error": noManagerText}},
			wantKind: "mls_no_manager_agent", wantMessage: "No member agent holds manage", wantHint: "--actions manage",
		},
		{
			name: "silent empty listen", args: listenArgs, fake: fakeChannel{status: http.StatusOK},
			wantKind: "channel_listen_closed", wantMessage: "listen stream ended", wantHint: "subscribe grant",
		},
		{
			name: "silent empty listen on an active channel", args: listenArgs, fake: fakeChannel{status: http.StatusOK, provisioning: map[string]any{"phase": "active"}},
			wantKind: "channel_permission_denied", wantMessage: "missing the subscribe grant", wantHint: "--actions subscribe`",
		},
		{
			name: "coordinator denies publish", args: publishArgs,
			fake:     fakeChannel{status: http.StatusForbidden, body: daemonError("permission_denied", "BTR authorization denied", nil)},
			wantKind: "channel_permission_denied", wantMessage: "not allowed", wantHint: "--actions publish`",
		},
		{
			name: "nobody online to receive", args: publishArgs,
			fake:     fakeChannel{status: http.StatusServiceUnavailable, body: daemonError("no_online_subscriber", "no member with Subscribe on this channel is online", map[string]any{"retryable": true})},
			wantKind: "channel_no_recipient", wantMessage: "is online", wantHint: "retention none",
		},
		{
			name: "nobody allowed to receive", args: publishArgs,
			fake:     fakeChannel{status: http.StatusServiceUnavailable, body: daemonError("no_authorized_subscriber", "no other member holds Subscribe on this channel", nil)},
			wantKind: "channel_no_subscriber", wantMessage: "holds subscribe", wantHint: "--actions subscribe`",
		},
		{
			name: "typed controller code with managers offline", args: publishArgs,
			fake:     fakeChannel{status: http.StatusServiceUnavailable, body: daemonError("mls_delivery_controller_unavailable", managersOfflineText, nil)},
			wantKind: "mls_managers_offline", wantMessage: "offline", wantHint: "beam agent start",
		},
	})
}

// A member with publish or subscribe but no discover on a restricted channel
// cannot see it, and the coordinator reports it as not found (D3).
func TestExecuteExplainsHiddenChannel(t *testing.T) {
	runChannelCases(t, []channelCase{
		{
			name: "publish reports encryption not ready on a hidden channel", args: publishArgs,
			fake:     fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable, hidden: true, provisioning: map[string]any{"phase": "bootstrapping"}},
			wantKind: "channel_not_visible", wantMessage: "lacks the discover grant", wantHint: "--actions discover,publish,subscribe`",
			avoid: []string{"encryption is not ready"},
		},
		{
			name: "listen ends silently on a hidden channel", args: listenArgs, fake: fakeChannel{status: http.StatusOK, hidden: true},
			wantKind: "channel_not_visible", wantMessage: "lacks the discover grant", wantHint: "`beam room channel list btr_room_a`",
		},
		{
			name: "coordinator reports the channel missing", args: publishArgs,
			fake:     fakeChannel{status: http.StatusNotFound, body: daemonError("not_found", "BTR resource not found", nil)},
			wantKind: "channel_not_visible", wantMessage: "discover grant", wantHint: "BTR authorization denied",
		},
		{
			name: "grant list by a member without manage", args: []string{"grant", "list", "btr_room_a", "btr_channel_a"},
			fake:     fakeChannel{status: http.StatusNotFound, body: daemonError("not_found", "BTR resource not found", nil)},
			wantKind: "channel_grants_not_visible", wantMessage: "not visible to this member", wantHint: "`beam room channel list btr_room_a`",
		},
	})
}

// The daemon may forward the coordinator's controller_unavailable reason in
// several shapes; each must be read, and preferred over the phase fallback.
func TestExecuteReadsForwardedControllerReason(t *testing.T) {
	bootstrapping := map[string]any{"phase": "bootstrapping"}
	runChannelCases(t, []channelCase{
		{
			name: "typed code with details.reason", args: publishArgs,
			fake: fakeChannel{status: http.StatusServiceUnavailable, provisioning: bootstrapping,
				body: daemonError("mls_delivery_controller_unavailable", "no eligible MLS delivery controller", map[string]any{"details": map[string]any{"reason": "no_manager_agent", "channel_id": "btr_channel_a"}})},
			wantKind: "mls_no_manager_agent", wantMessage: "No member agent holds manage", wantHint: "--actions manage",
		},
		{
			name: "generic publish_failed with a top-level reason", args: publishArgs,
			fake: fakeChannel{status: http.StatusBadRequest, provisioning: bootstrapping,
				body: daemonError("publish_failed", "MLS channel state is unavailable", map[string]any{"reason": "managers_offline"})},
			wantKind: "mls_managers_offline", wantMessage: "offline", wantHint: "beam agent start",
		},
		{
			name: "controller_unavailable nested in details", args: publishArgs,
			fake: fakeChannel{status: http.StatusBadRequest, provisioning: bootstrapping,
				body: daemonError("publish_failed", "MLS channel state is unavailable", map[string]any{"details": map[string]any{"controller_unavailable": controllerUnavailable("no_manager_agent", noManagerText)}})},
			wantKind: "mls_no_manager_agent", wantMessage: "No member agent holds manage", wantHint: "--actions manage",
		},
		{
			name: "controller_unavailable in the error object", args: publishArgs,
			fake: fakeChannel{status: http.StatusBadRequest, provisioning: bootstrapping,
				body: daemonError("publish_failed", "MLS channel state is unavailable", map[string]any{"controller_unavailable": controllerUnavailable("managers_offline", managersOfflineText)})},
			wantKind: "mls_managers_offline", wantMessage: "offline", wantHint: "beam agent start",
		},
		{
			name: "controller_unavailable in provisioning state", args: publishArgs,
			fake: fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable,
				provisioning: map[string]any{"phase": "bootstrapping", "controller_unavailable": controllerUnavailable("managers_offline", managersOfflineText)}},
			wantKind: "mls_managers_offline", wantMessage: "offline", wantHint: "beam agent start",
		},
		{
			name: "code and reason at the top of provisioning state", args: listenArgs,
			fake: fakeChannel{status: http.StatusOK,
				provisioning: map[string]any{"phase": "bootstrapping", "code": "mls_delivery_controller_unavailable", "reason": "no_manager_agent"}},
			wantKind: "mls_no_manager_agent", wantMessage: "No member agent holds manage", wantHint: "--actions manage",
		},
		{
			name: "controller_unavailable on the channel status", args: publishArgs,
			fake: fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable, provisioning: map[string]any{"phase": "active"},
				channel: map[string]any{"channel": map[string]any{"channel_id": "btr_channel_a", "controller_unavailable": controllerUnavailable("no_manager_agent", noManagerText)}}},
			wantKind: "mls_no_manager_agent", wantMessage: "No member agent holds manage", wantHint: "--actions manage",
		},
	})
}

// An older agent returns 504 channel_setup_timeout when no Worker claims the
// workload; the runtime fix reports worker_capability_unavailable (D4).
func TestExecuteExplainsWorkerUnavailable(t *testing.T) {
	timeout := func(phase, state string) map[string]any {
		return daemonError("channel_setup_timeout", "room workload "+phase+" timed out after 30s (room=btr_room_a channel=btr_channel_a workload=btr_wl_1 last_state="+state+")", map[string]any{"retryable": true})
	}
	runChannelCases(t, []channelCase{
		{
			name: "publish never claimed", args: publishArgs, fake: fakeChannel{status: http.StatusGatewayTimeout, body: timeout("provisioning", "submitted")},
			wantKind: "channel_setup_timeout", wantMessage: "no Beam delivery Worker picked it up in time", wantHint: "no Worker available",
			avoid: []string{"beam agent logs"},
		},
		{
			name: "listen never claimed", args: listenArgs, fake: fakeChannel{status: http.StatusGatewayTimeout, body: timeout("submit", "unknown")},
			wantKind: "channel_setup_timeout", wantMessage: "no Beam Worker picked the request up", wantHint: "retry",
		},
		{
			name: "transfer stalled after a Worker claimed it", args: publishArgs, fake: fakeChannel{status: http.StatusGatewayTimeout, body: timeout("transfer", "running")},
			wantKind: "channel_setup_timeout", wantMessage: "did not finish", wantHint: "beam agent logs",
		},
		{
			name: "runtime reports no worker", args: publishArgs,
			fake:     fakeChannel{status: http.StatusServiceUnavailable, body: daemonError("worker_capability_unavailable", "no worker available for room.message.direct.v1 (no capable orchestrator)", map[string]any{"retryable": true, "capability": "room.message.direct.v1"})},
			wantKind: "worker_capability_unavailable", wantMessage: "No Beam Worker is available", wantHint: "no Worker available",
		},
		{
			name: "runtime reason text only", args: publishArgs,
			fake:     fakeChannel{status: http.StatusServiceUnavailable, body: daemonError("unavailable", "no worker available for room.message.direct.v1 (no capable orchestrator)", nil)},
			wantKind: "worker_capability_unavailable", wantMessage: "No Beam Worker is available", wantHint: "retry",
		},
	})
}

// A failed stream lease or a datagram workload that ended before its source
// was read carries the reasons its destinations gave; an unavailable datagram
// relay is explained, and any other reason stays visible in the agent's
// message.
func TestExecuteExplainsStreamAndDatagramFailureReasons(t *testing.T) {
	streamPublishArgs := []string{"channel", "stream", "btr_room_a", "publish", "btr_channel_a"}
	datagramSendArgs := []string{"channel", "datagram", "btr_room_a", "send", "btr_channel_a", "--literal", "event"}
	settled := func(code, message string, reasons ...string) map[string]any {
		if len(reasons) != 0 {
			message += ": " + strings.Join(reasons, ", ")
		}
		message += " (room=btr_room_a channel=btr_channel_a workload=btr_flow_1)"
		var extra map[string]any
		if len(reasons) != 0 {
			list := make([]any, 0, len(reasons))
			for _, reason := range reasons {
				list = append(list, reason)
			}
			extra = map[string]any{"details": map[string]any{"reason": reasons[0], "reasons": list}}
		}
		return daemonError(code, message, extra)
	}
	const (
		leaseFailed  = "room workload ended in state failed"
		unreadFailed = "room workload ended in state failed before its source was read"
	)
	runChannelCases(t, []channelCase{
		{
			name: "datagram relay unavailable", args: datagramSendArgs,
			fake:     fakeChannel{status: http.StatusBadRequest, body: settled("datagram_failed", unreadFailed, "room_workload_relay_unavailable")},
			wantKind: "datagram_relay_unavailable", wantMessage: "The datagram could not be sent: Beam has no relay available for it right now.",
			wantHint: "Datagram channels currently depend on Beam relays",
		},
		{
			// Streams are served by Workers, so a relay reason on a stream
			// is not explained as a missing relay.
			name: "relay reason on a stream is not explained", args: streamPublishArgs,
			fake:        fakeChannel{status: http.StatusBadRequest, body: settled("stream_failed", leaseFailed, "room_workload_relay_unavailable")},
			wantMessage: "ended in state failed: room_workload_relay_unavailable (room=btr_room_a",
			avoid:       []string{"relay available", "depend on Beam relays"},
		},
		{
			name: "unknown reason stays in the message", args: streamPublishArgs,
			fake:        fakeChannel{status: http.StatusBadRequest, body: settled("stream_failed", leaseFailed, "attempt_watchdog_expired", "source_lost")},
			wantMessage: "ended in state failed: attempt_watchdog_expired, source_lost (room=btr_room_a",
		},
		{
			name: "no reason keeps the message", args: streamPublishArgs,
			fake:        fakeChannel{status: http.StatusBadRequest, body: settled("stream_failed", leaseFailed)},
			wantMessage: "room workload ended in state failed (room=btr_room_a",
		},
		{
			// Only a datagram workload is given a relay.
			name: "relay reason on another command is not explained", args: publishArgs,
			fake:        fakeChannel{status: http.StatusBadRequest, body: settled("publish_failed", unreadFailed, "room_workload_relay_unavailable")},
			wantMessage: "room_workload_relay_unavailable",
		},
	})
}

// Worker-served streams: the agent's stream failure codes and the
// coordinator's capability refusals are explained by code, or by the
// coordinator's text when only that is forwarded, and keep their exit codes.
func TestExecuteExplainsStreamFailures(t *testing.T) {
	publish := []string{"channel", "stream", "btr_room_a", "publish", "btr_channel_a"}
	listen := []string{"channel", "stream", "btr_room_a", "listen", "btr_channel_a"}
	withReasons := func(code, message string, reasons ...string) map[string]any {
		list := make([]any, 0, len(reasons))
		for _, reason := range reasons {
			list = append(list, reason)
		}
		if len(reasons) != 0 {
			message += ": " + strings.Join(reasons, ", ")
		}
		return daemonError(code, message, map[string]any{"details": map[string]any{"reason": reasons[0], "reasons": list}})
	}
	retryable := map[string]any{"retryable": true}
	for _, test := range []struct {
		name        string
		args        []string
		status      int
		body        any
		wantCode    int
		wantKind    string
		wantMessage string
		wantHint    string
		avoid       []string
	}{
		{
			name: "source agent without stream support", args: publish, status: http.StatusServiceUnavailable,
			body:     daemonError("agent_capability_unavailable", "source agent is workload-incapable: it does not advertise room.stream.direct.v1", retryable),
			wantCode: ExitFailed, wantKind: "agent_capability_unavailable",
			wantMessage: "does not support Worker-served streams (room.stream.direct.v1)", wantHint: "Update the Beam CLI and agent on every member's machine",
			avoid: []string{"workload-incapable"},
		},
		{
			name: "target agent without stream support, phrase only", args: publish, status: http.StatusServiceUnavailable,
			body:     daemonError("unavailable", "no online target agent advertises room.stream.direct.v1", nil),
			wantCode: ExitFailed, wantKind: "agent_capability_unavailable",
			wantMessage: "does not support Worker-served streams", wantHint: "`beam update`",
		},
		{
			// Another missing capability keeps the generic explanation.
			name: "agent without the stream kind", args: publish, status: http.StatusServiceUnavailable,
			body:     daemonError("agent_capability_unavailable", "source agent is workload-incapable: it does not advertise room.stream", retryable),
			wantCode: ExitFailed, wantKind: "agent_capability_unavailable",
			wantMessage: "it does not advertise room.stream", wantHint: "Run `beam update` on that machine",
			avoid: []string{"Worker-served"},
		},
		{
			name: "no Worker offers streams", args: publish, status: http.StatusServiceUnavailable,
			body:     daemonError("worker_capability_unavailable", "no worker available for room.stream.direct.v1 (no capable orchestrator)", retryable),
			wantCode: ExitFailed, wantKind: "worker_capability_unavailable",
			wantMessage: "No Beam Worker offers streams right now", wantHint: "retry",
		},
		{
			name: "no Worker offers streams, phrase only", args: publish, status: http.StatusServiceUnavailable,
			body:     daemonError("unavailable", "no worker available for room.stream.direct.v1 (no capable orchestrator)", nil),
			wantCode: ExitFailed, wantKind: "worker_capability_unavailable",
			wantMessage: "No Beam Worker offers streams right now", wantHint: "retry",
		},
		{
			name: "stream key unavailable", args: listen, status: http.StatusServiceUnavailable,
			body:     daemonError("stream_key_unavailable", "the stream channel's MLS key epoch is unavailable", retryable),
			wantCode: ExitFailed, wantKind: "stream_key_unavailable",
			wantMessage: "could not get the channel's current encryption key", wantHint: "end-to-end encrypted",
			avoid: []string{"key epoch"},
		},
		{
			// The reason names the coordinator code, which must not be read
			// as the Worker refusal itself.
			name: "renewal failed", args: publish, status: http.StatusServiceUnavailable,
			body:     withReasons("stream_renewal_failed", "no continuation lease was provisioned before the room stream lease expired", "worker_capability_unavailable"),
			wantCode: ExitFailed, wantKind: "stream_renewal_failed",
			wantMessage: "could not renew its Worker lease before it expired (reason: worker_capability_unavailable).", wantHint: "publish the stream again",
		},
		{
			name: "renewal failed without a reason", args: publish, status: http.StatusServiceUnavailable,
			body:     daemonError("stream_renewal_failed", "no continuation lease was provisioned before the room stream lease expired", retryable),
			wantCode: ExitFailed, wantKind: "stream_renewal_failed",
			wantMessage: "could not renew its Worker lease before it expired.", wantHint: "one-hour Worker leases",
			avoid: []string{"(reason"},
		},
		{
			name: "every target dropped out", args: publish, status: http.StatusConflict,
			body:     withReasons("no_active_targets", "every target of the room stream ended before its source", "no_live_subscriber", "target_heartbeat_timeout"),
			wantCode: ExitConflict, wantKind: "no_active_targets",
			wantMessage: "every member receiving it dropped out before it ended (reasons: no_live_subscriber, target_heartbeat_timeout).", wantHint: "`stream listen`",
		},
		{
			name: "listened stream truncated", args: listen, status: http.StatusBadGateway,
			body:     withReasons("stream_truncated", "the room stream ended before its end of stream", "source_aborted"),
			wantCode: ExitFailed, wantKind: "stream_truncated",
			wantMessage: "the data received is incomplete (reason: source_aborted).", wantHint: "source_aborted or source_lost",
		},
		{
			// Stream failure codes are explained only for stream commands.
			name: "stream code on a message publish", args: publishArgs, status: http.StatusConflict,
			body:     daemonError("no_active_targets", "every target ended", nil),
			wantCode: ExitConflict, wantMessage: "every target ended",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := failingRunner(t, test.status, test.body)
			failure := runner.Execute(context.Background(), append([]string{"tunnel", "room"}, test.args...))
			if failure == nil {
				t.Fatal("command succeeded")
			}
			if failure.Code != test.wantCode || failure.Kind != test.wantKind {
				t.Fatalf("failure=%+v, want code %d kind %q", failure, test.wantCode, test.wantKind)
			}
			if !strings.Contains(failure.Message, test.wantMessage) || !strings.Contains(failure.Hint, test.wantHint) {
				t.Fatalf("failure=%+v, want message %q and hint %q", failure, test.wantMessage, test.wantHint)
			}
			for _, avoid := range test.avoid {
				if strings.Contains(failure.Message+failure.Hint, avoid) {
					t.Fatalf("failure=%+v must not contain %q", failure, avoid)
				}
			}
		})
	}
}

// An older agent fails a live-only publish with 410 room_workload_expired even
// when some members received it (D6); the counts decide the wording.
func TestExecuteExplainsPartialLiveOnlyDelivery(t *testing.T) {
	expired := daemonError("room_workload_expired", "room workload ended in state expired (room=btr_room_a channel=btr_channel_a workload=btr_wl_1)", nil)
	destinations := func(states ...string) map[string]any {
		list := make([]any, 0, len(states))
		for index, state := range states {
			list = append(list, map[string]any{"target_member_id": "btr_member_" + string(rune('b'+index)), "status": state})
		}
		return map[string]any{"workload": map[string]any{"workload_id": "btr_wl_1", "status": "expired", "destinations": list}}
	}
	runChannelCases(t, []channelCase{
		{
			name: "one listening, one not, from the workload status", args: publishArgs,
			fake:     fakeChannel{status: http.StatusGone, body: expired, workload: destinations("completed", "provisioning")},
			wantKind: "channel_partially_delivered", wantMessage: "Delivered to 1 member; 1 member was not listening and will never get it", wantHint: "Start `listen` on the other members",
			avoid: []string{"No member received"},
		},
		{
			name: "counts in the workload details", args: publishArgs,
			fake:     fakeChannel{status: http.StatusGone, body: expired, workload: map[string]any{"workload": map[string]any{"status": "expired", "details": map[string]any{"delivered": 2, "pending": 1}}}},
			wantKind: "channel_partially_delivered", wantMessage: "Delivered to 2 members; 1 member was not listening",
		},
		{
			name: "counts in the error body", args: publishArgs,
			fake: fakeChannel{status: http.StatusGone,
				body: daemonError("room_workload_expired", "room workload ended in state expired", map[string]any{"details": map[string]any{"online_deliveries": 3, "delivered_deliveries": 1}})},
			wantKind: "channel_partially_delivered", wantMessage: "Delivered to 1 member; 2 members were not listening",
		},
		{
			name: "nobody listening", args: publishArgs,
			fake:     fakeChannel{status: http.StatusGone, body: expired, workload: destinations("provisioning", "provisioning")},
			wantKind: "channel_not_delivered", wantMessage: "No member received this message", wantHint: "start `listen`",
		},
		{
			name: "failed workload without counts", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: daemonError("room_workload_failed", "room workload ended in state failed", nil)},
			wantKind: "channel_not_delivered", wantMessage: "not delivered to every member", wantHint: "start `listen`",
			avoid: []string{"No member received"},
		},
	})
}

// A live-only publish whose delivery failed for a listening member must not be
// reported as nobody listening (D10).
func TestExecuteExplainsFailedLiveOnlyDelivery(t *testing.T) {
	failed := func(details map[string]any, extra map[string]any) map[string]any {
		fields := map[string]any{"details": details}
		for key, value := range extra {
			fields[key] = value
		}
		return daemonError("room_workload_failed", "room workload ended in state failed (room=btr_room_a channel=btr_channel_a workload=btr_wl_1)", fields)
	}
	counts := func(online, delivered, failedCount, skipped int) map[string]any {
		return map[string]any{"online_deliveries": online, "delivered_deliveries": delivered, "failed_deliveries": failedCount, "skipped_online_only": skipped}
	}
	withKeys := func(details map[string]any, extra map[string]any) map[string]any {
		for key, value := range extra {
			details[key] = value
		}
		return details
	}
	type destination struct{ member, status, reason string }
	workload := func(list ...destination) map[string]any {
		destinations := make([]any, 0, len(list))
		for _, item := range list {
			entry := map[string]any{"target_member_id": item.member, "status": item.status, "reason": nil}
			if item.reason != "" {
				entry["reason"] = item.reason
			}
			destinations = append(destinations, entry)
		}
		return map[string]any{"workload": map[string]any{"workload_id": "btr_wl_1", "status": "failed", "destinations": destinations}}
	}
	notNobodyListening := []string{"none was listening", "No member received this message"}
	runChannelCases(t, []channelCase{
		{
			name: "all failed", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: failed(counts(1, 0, 1, 0), nil)},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 1 listening member. No member received it.", wantHint: "publish again to retry",
			avoid: append([]string{"not listening", "get it again"}, notNobodyListening...),
		},
		{
			// The exact D10 report: B listening but unable to decrypt, C not listening.
			name: "failed and not listening", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: failed(counts(2, 0, 1, 1), nil)},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 1 listening member. No member received it; 1 member was not listening", wantHint: "`beam agent logs` on the receiving member",
			avoid: notNobodyListening,
		},
		{
			name: "delivered and failed", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: failed(counts(3, 1, 2, 0), nil)},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 2 listening members. It was delivered to 1 member.", wantHint: "get it again",
			avoid: []string{"not listening"},
		},
		{
			name: "failed members and reason in the details", args: publishArgs,
			fake: fakeChannel{status: http.StatusConflict, body: failed(withKeys(counts(2, 0, 1, 1), map[string]any{
				"failed_member_ids": []any{"btr_member_b"}, "failure_reason": "MLS decryption failed"}), nil)},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 1 listening member (btr_member_b): MLS decryption failed.", wantHint: "beam agent logs",
		},
		{
			name: "reason in details.reason", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: failed(withKeys(counts(1, 0, 1, 0), map[string]any{"reason": "decrypt_failed"}), nil)},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 1 listening member: decrypt_failed.",
		},
		{
			name: "reason beside the details", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: failed(counts(1, 0, 1, 0), map[string]any{"failure_reason": "decrypt_failed", "failed_member_ids": []any{"btr_member_b"}})},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 1 listening member (btr_member_b): decrypt_failed.",
		},
		{
			name: "per-destination statuses from the workload", args: publishArgs,
			fake: fakeChannel{status: http.StatusConflict, body: daemonError("room_workload_failed", "room workload ended in state failed (room=btr_room_a channel=btr_channel_a workload=btr_wl_1)", nil),
				workload: workload(
					destination{"btr_member_b", "completed", ""},
					destination{"btr_member_c", "failed", "mls_decrypt_failed"},
					destination{"btr_member_d", "failed", "no_live_subscriber"},
					destination{"btr_member_e", "unavailable", "expired"},
				)},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 1 listening member (btr_member_c): mls_decrypt_failed. It was delivered to 1 member; 2 members were not listening",
			wantHint: "need `listen` running",
		},
		{
			name: "per-destination not listening only", args: publishArgs,
			fake: fakeChannel{status: http.StatusGone, body: daemonError("room_workload_expired", "room workload ended in state expired (room=btr_room_a channel=btr_channel_a workload=btr_wl_1)", nil),
				workload: workload(destination{"btr_member_b", "failed", "no_live_subscriber"}, destination{"btr_member_c", "provisioning", ""})},
			wantKind: "channel_not_delivered", wantMessage: "none was listening", wantHint: "start `listen`",
		},
		{
			// Older agents send the error text instead of the reason token.
			name: "per-destination not listening from an older agent", args: publishArgs,
			fake: fakeChannel{status: http.StatusGone, body: daemonError("room_workload_expired", "room workload ended in state expired (room=btr_room_a channel=btr_channel_a workload=btr_wl_1)", nil),
				workload: workload(destination{"btr_member_b", "completed", ""}, destination{"btr_member_c", "failed", "direct room message target has no live subscriber"})},
			wantKind: "channel_partially_delivered", wantMessage: "Delivered to 1 member; 1 member was not listening",
			avoid: []string{"Delivery failed"},
		},
		{
			// A member whose agent never answered may have the
			// message, so it is neither "none was listening" nor "No member
			// received it", and publishing again is not offered as a retry.
			name: "nobody confirmed it and a member never answered", args: publishArgs,
			fake: fakeChannel{status: http.StatusConflict, body: daemonError("delivery_unconfirmed",
				"no member confirmed receiving the message, and 1 did not answer (room=btr_room_a channel=btr_channel_a workload=btr_wl_1)",
				map[string]any{"details": map[string]any{"online_deliveries": 2, "delivered_deliveries": 0,
					"failed_deliveries": 0, "skipped_online_only": 1, "expired_deliveries": 1,
					"unconfirmed_member_ids": []any{"btr_member_b"}, "failure_reason": "delivery_unconfirmed",
					"member_failures": []any{map[string]any{"member_id": "btr_member_b", "reason": "delivery_unconfirmed"}}}})},
			wantKind:    "channel_delivery_unconfirmed",
			wantMessage: "No member confirmed receiving this message: 1 member (btr_member_b) did not answer, so they may have it. 1 member was not listening.",
			wantHint:    "Publishing again can deliver the message twice",
			avoid:       append([]string{"listening member", "No member received it", "publish again to retry"}, notNobodyListening...),
		},
		{
			// The members that answered are named apart from those
			// that never did.
			name: "nobody confirmed it, one never answered and one failed", args: publishArgs,
			fake: fakeChannel{status: http.StatusConflict, body: daemonError("delivery_unconfirmed",
				"no member confirmed receiving the message, and 2 did not answer (room=btr_room_a channel=btr_channel_a workload=btr_wl_1)",
				map[string]any{"details": map[string]any{"online_deliveries": 3, "delivered_deliveries": 0,
					"failed_deliveries": 1, "skipped_online_only": 0, "expired_deliveries": 2,
					"failed_member_ids": []any{"btr_member_c"}, "unconfirmed_member_ids": []any{"btr_member_b", "btr_member_d"},
					"failure_reason": "stale_scope",
					"member_failures": []any{
						map[string]any{"member_id": "btr_member_b", "reason": "delivery_unconfirmed"},
						map[string]any{"member_id": "btr_member_c", "reason": "stale_scope"},
						map[string]any{"member_id": "btr_member_d", "reason": "delivery_unconfirmed"}}}})},
			wantKind: "channel_delivery_unconfirmed",
			wantMessage: "No member confirmed receiving this message: 2 members (btr_member_b, btr_member_d) did not answer, so they may have it. " +
				"Delivery failed for 1 listening member (btr_member_c): the channel was re-keyed while the message was in flight.",
			wantHint: "Publishing again can deliver the message twice",
			avoid:    append([]string{"No member received it", "nothing was sent"}, notNobodyListening...),
		},
		{
			name: "nobody received it because the channel re-keyed in flight", args: publishArgs,
			fake: fakeChannel{status: http.StatusConflict, body: failed(withKeys(counts(1, 0, 1, 0), map[string]any{
				"failed_member_ids": []any{"btr_member_b"}, "failure_reason": "stale_scope"}), nil)},
			wantKind:    "channel_rekeying",
			wantMessage: "Delivery failed for 1 listening member (btr_member_b): the channel was re-keyed while the message was in flight.",
			wantHint:    "Retry in a moment",
			avoid:       notNobodyListening,
		},
		{
			// An agent must report such a publish as an error; one that
			// reports a success with nobody reached still fails the command.
			name: "success result with nobody reached and a member unconfirmed", args: publishArgs,
			fake: fakeChannel{status: http.StatusOK, body: map[string]any{"publication_id": "btr_pub_1", "online_deliveries": 2,
				"accepted_deliveries": 0, "delivered_deliveries": 0, "failed_deliveries": 0, "expired_deliveries": 1, "skipped_online_only": 1}},
			wantKind:    "channel_delivery_unconfirmed",
			wantMessage: "No member confirmed receiving this message: 1 member did not answer, so they may have it. 1 member was not listening.",
			avoid:       append([]string{"No member received it"}, notNobodyListening...),
		},
		{
			name: "success result with nobody reached and a failed delivery", args: publishArgs,
			fake: fakeChannel{status: http.StatusOK, body: map[string]any{"publication_id": "btr_pub_1", "online_deliveries": 1,
				"accepted_deliveries": 0, "delivered_deliveries": 0, "failed_deliveries": 1, "skipped_online_only": 0}},
			wantKind: "channel_delivery_failed", wantMessage: "Delivery failed for 1 listening member. No member received it.",
		},
		{
			name: "older daemon without a failed count", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: failed(map[string]any{"online_deliveries": 2, "delivered_deliveries": 0}, nil)},
			wantKind: "channel_not_delivered", wantMessage: "No member received this message: none was listening", wantHint: "start `listen`",
		},
		{
			name: "counts that account for nobody stay neutral", args: publishArgs,
			fake:     fakeChannel{status: http.StatusConflict, body: failed(counts(0, 0, 0, 0), nil)},
			wantKind: "channel_not_delivered", wantMessage: "not delivered to every member", wantHint: "start `listen`",
			avoid: notNobodyListening,
		},
	})
}

// A live-only publish that reached nobody exits non-zero unless every
// targeted member said it was not listening.
func TestLiveOnlyPublishReachingNobodySucceedsOnlyWhenNobodyListened(t *testing.T) {
	for _, test := range []struct {
		name string
		body map[string]any
		want int
	}{
		{name: "nobody listening", want: ExitOK, body: map[string]any{"publication_id": "btr_pub_1", "online_deliveries": 2,
			"delivered_deliveries": 0, "failed_deliveries": 0, "skipped_online_only": 2}},
		{name: "a member never answered", want: ExitFailed, body: map[string]any{"publication_id": "btr_pub_1", "online_deliveries": 2,
			"delivered_deliveries": 0, "failed_deliveries": 0, "expired_deliveries": 1, "skipped_online_only": 1}},
		{name: "delivered with one unconfirmed", want: ExitOK, body: map[string]any{"publication_id": "btr_pub_1", "online_deliveries": 2,
			"delivered_deliveries": 1, "failed_deliveries": 0, "expired_deliveries": 1, "skipped_online_only": 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := failingRunner(t, http.StatusOK, test.body)
			runner.Notices = &bytes.Buffer{}
			if code := runner.Run(context.Background(), []string{"tunnel", "room", "channel", "publish", "btr_room_a", "btr_channel_a", "--literal", "hi"}); code != test.want {
				t.Fatalf("exit=%d want %d", code, test.want)
			}
		})
	}
}

func TestFailureReasonIsSanitized(t *testing.T) {
	if got := failureReason("  MLS\tdecryption\n\x1b[31mfailed\r "); got != "MLS decryption [31mfailed" {
		t.Fatalf("reason=%q", got)
	}
	long := failureReason(strings.Repeat("x", 500))
	if len([]rune(long)) != maxFailureReasonLength || !strings.HasSuffix(long, "...") {
		t.Fatalf("long reason=%q (%d runes)", long, len([]rune(long)))
	}
	if got := memberList([]string{"a", "b", "c", "d", "e"}); got != "a, b, c and 2 more" {
		t.Fatalf("members=%q", got)
	}
}

// syncBuffer is a bytes.Buffer safe to read while a command writes to it.
type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// The live-only notice must show while listen waits for its first message,
// even from an agent that holds the response headers until then (D2).
func TestListenNoticeShowsBeforeTheFirstMessage(t *testing.T) {
	for _, test := range []struct {
		name   string
		flags  []string
		notice bool
	}{
		{name: "human", notice: true},
		{name: "json stays silent", flags: []string{"--json"}},
		{name: "quiet stays silent", flags: []string{"--quiet"}},
		{name: "receiver_local has no live-only notice", flags: []string{"--retention", "receiver_local"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requested := make(chan struct{})
			release := make(chan struct{})
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(requested)
				<-release
				w.Header().Set("Content-Type", "application/x-ndjson")
				_ = json.NewEncoder(w).Encode(map[string]any{"publication_id": "btr_pub_1", "text": "hello"})
			}))
			var stdout bytes.Buffer
			var notices syncBuffer
			runner := Runner{Client: tunnel.NewLocalClient(socket), Out: &stdout, Err: &bytes.Buffer{}, Notices: &notices}
			done := make(chan int, 1)
			go func() {
				done <- runner.Run(context.Background(), append([]string{"tunnel", "room", "channel", "listen", "btr_room_a", "btr_channel_a"}, test.flags...))
			}()
			<-requested
			// The agent has not answered yet: the notice must already be out.
			if got := strings.Contains(notices.String(), "listening live only"); got != test.notice {
				close(release)
				t.Fatalf("notice while waiting=%v want %v (%q)", got, test.notice, notices.String())
			}
			close(release)
			if code := <-done; code != ExitOK {
				t.Fatalf("code=%d", code)
			}
			if count := strings.Count(notices.String(), "listening live only"); count > 1 {
				t.Fatalf("notice printed %d times: %q", count, notices.String())
			}
			if strings.Contains(stdout.String(), "live only") {
				t.Fatalf("notice leaked into stdout: %q", stdout.String())
			}
		})
	}
}

func TestExecuteExplainsCoordinatorTransportErrors(t *testing.T) {
	publish := []string{"channel", "publish", "btr_room_a", "btr_channel_a", "--literal", "hi"}
	for _, test := range []struct {
		name        string
		args        []string
		code        string
		message     string
		wantKind    string
		wantMessage string
	}{
		{
			name: "client timeout on publish", args: publish, code: "publish_failed",
			message:  `Post "https://coord.example.com/v1/rooms/btr_room_a/channels/btr_channel_a/publish?token=secret123&sig=abc": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
			wantKind: "coordinator_timeout", wantMessage: "The coordinator did not answer in time (POST https://coord.example.com/v1/rooms/btr_room_a/channels/btr_channel_a/publish: timed out)",
		},
		{
			name: "prefixed context deadline", args: publish, code: "publish_failed",
			message:  `publish room message: Post "https://coord.example.com/v1/publish": context deadline exceeded`,
			wantKind: "coordinator_timeout", wantMessage: "did not answer in time",
		},
		{
			name: "connection refused", args: []string{"inspect", "btr_room_a"}, code: "internal",
			message:  `Get "http://192.0.2.5:8080/v1/rooms/btr_room_a": dial tcp 192.0.2.5:8080: connect: connection refused`,
			wantKind: "coordinator_unreachable", wantMessage: "could not be reached (GET http://192.0.2.5:8080/v1/rooms/btr_room_a: connection refused)",
		},
		{
			name: "connection reset", args: publish, code: "publish_failed",
			message:  `Post "https://coord.example.com/v1/publish": read tcp 192.0.2.2:5555->192.0.2.5:443: read: connection reset by peer`,
			wantKind: "coordinator_unreachable", wantMessage: "connection reset",
		},
		{
			name: "EOF", args: publish, code: "publish_failed",
			message:  `Post "https://coord.example.com/v1/publish": EOF`,
			wantKind: "coordinator_unreachable", wantMessage: "connection closed before a response",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := failingRunner(t, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"code": test.code, "message": test.message},
			})
			failure := runner.Execute(context.Background(), append([]string{"tunnel", "room"}, test.args...))
			if failure == nil {
				t.Fatal("command succeeded")
			}
			if failure.Kind != test.wantKind {
				t.Fatalf("kind=%q want %q (failure=%+v)", failure.Kind, test.wantKind, failure)
			}
			if !strings.Contains(failure.Message, test.wantMessage) || !strings.Contains(failure.Message, "can be retried") {
				t.Fatalf("message=%q want %q", failure.Message, test.wantMessage)
			}
			for _, leaked := range []string{"secret123", "sig=", "?", "Client.Timeout", "dial tcp"} {
				if strings.Contains(failure.Message, leaked) {
					t.Fatalf("message=%q leaks %q", failure.Message, leaked)
				}
			}
			for _, want := range []string{"retry", "`beam agent status`", "network access to the coordinator"} {
				if !strings.Contains(failure.Hint, want) {
					t.Fatalf("hint=%q want %q", failure.Hint, want)
				}
			}
		})
	}
}

func TestCoordinatorTransportKeepsExistingMappings(t *testing.T) {
	transport := `Post "https://coord.example.com/v1/publish": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`
	for _, test := range []struct {
		name     string
		err      *tunnel.Error
		wantKind string
		wantOK   bool
	}{
		{name: "typed code wins", err: &tunnel.Error{Code: "channel_setup_timeout", Detail: transport}, wantKind: "channel_setup_timeout", wantOK: true},
		{name: "phrase rule wins", err: &tunnel.Error{Code: "publish_failed", Detail: "no worker available: " + transport}, wantKind: "worker_capability_unavailable", wantOK: true},
		{name: "wrapped coordinator error", err: &tunnel.Error{Code: "publish_failed", Detail: "publish failed", Details: map[string]any{"error": transport}}, wantKind: "coordinator_timeout", wantOK: true},
		{name: "other client error stays raw", err: &tunnel.Error{Code: "publish_failed", Detail: `Post "https://coord.example.com/v1/publish": x509: certificate signed by unknown authority`}},
		{name: "timeout without a request stays raw", err: &tunnel.Error{Code: "publish_failed", Detail: "context deadline exceeded"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			known, ok := explainError("channel publish", test.err)
			if ok != test.wantOK || known.Kind != test.wantKind {
				t.Fatalf("explainError=(%+v, %v), want kind %q ok %v", known, ok, test.wantKind, test.wantOK)
			}
		})
	}
}

func TestRoomTransferRefusalsCarryTheAmounts(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         *tunnel.Error
		wantKind    string
		wantMessage string
	}{
		{
			name: "without credit",
			err: &tunnel.Error{Status: http.StatusPaymentRequired, Code: "insufficient_credits", Detail: "This organization does not have enough credits for this room transfer.",
				Fields: map[string]any{"credits_required": 2.5, "credits_available": 1.2}},
			wantKind:    "room_insufficient_credits",
			wantMessage: "This organization does not have enough credits for this room transfer (credits required: 2.5, available: 1.2).",
		},
		{
			name:        "without amounts",
			err:         &tunnel.Error{Status: http.StatusPaymentRequired, Code: "insufficient_credits", Detail: "This organization does not have enough credits for this room transfer."},
			wantKind:    "room_insufficient_credits",
			wantMessage: "This organization does not have enough credits for this room transfer.",
		},
		{
			name: "only the available balance",
			err: &tunnel.Error{Status: http.StatusPaymentRequired, Code: "insufficient_credits", Detail: "No credit.",
				Details: map[string]any{"credits_required": nil, "credits_available": -0.5}},
			wantKind:    "room_insufficient_credits",
			wantMessage: "No credit (credits available: -0.5).",
		},
		{
			name:        "blocked organization",
			err:         &tunnel.Error{Status: http.StatusForbidden, Code: "organization_blocked", Detail: "This organization cannot start transfers."},
			wantKind:    "room_organization_blocked",
			wantMessage: "This organization cannot start transfers.",
		},
		{
			name: "permission already named",
			err: &tunnel.Error{Status: http.StatusForbidden, Code: "permission_required", Detail: "API key lacks transfers:create.",
				Details: map[string]any{"permission": "transfers:create"}},
			wantKind:    "room_permission_required",
			wantMessage: "API key lacks transfers:create.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			known, ok := explainError("channel object", test.err)
			if !ok || known.Kind != test.wantKind {
				t.Fatalf("explainError=(%+v, %v), want kind %q", known, ok, test.wantKind)
			}
			message := known.Message
			if message == "" {
				message = test.err.Error()
			}
			if message != test.wantMessage {
				t.Fatalf("message=%q, want %q", message, test.wantMessage)
			}
		})
	}
	if known, ok := explainError("create", &tunnel.Error{Status: http.StatusPaymentRequired, Code: "CREDIT_REQUIRED", Detail: "Organization has no credits remaining"}); ok {
		t.Fatalf("credit_required is not a refusal code: %+v", known)
	}
}

func TestRedactedURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://coord.example.com/v1/rooms/a/publish?token=secret#frag": "https://coord.example.com/v1/rooms/a/publish",
		"https://user:pass@coord.example.com:8443/v1?x=1":                "https://coord.example.com:8443/v1",
		"http://coord.example.com":                                       "http://coord.example.com",
		"https://user:pa%zz@coord.example.com/v1?token=secret":           "https://coord.example.com",
	} {
		if got := redactedURL(raw); got != want {
			t.Fatalf("redactedURL(%q)=%q want %q", raw, got, want)
		}
	}
}

// A membership change re-keys a channel for a few seconds. Whatever the agent
// reports, the explanation names the re-key with how long this machine's
// agent has been in it, never a missing grant.
func TestExecuteExplainsChannelRekeying(t *testing.T) {
	rekeyingSince := time.Now().Add(-12 * time.Second).UTC().Format(time.RFC3339Nano)
	runChannelCases(t, []channelCase{
		{
			name: "agent gave up waiting for the new epoch", args: publishArgs,
			fake: fakeChannel{status: http.StatusServiceUnavailable, provisioning: map[string]any{"phase": "rekeying", "since": rekeyingSince},
				body: daemonError("channel_rekeying", "channel btr_channel_a is renewing its encryption keys after a membership change and was not ready after waiting 10s",
					map[string]any{"retryable": true, "details": map[string]any{"phase": "rekeying", "since": rekeyingSince, "waited_ms": 10000}})},
			wantKind: "channel_rekeying", wantMessage: "renewing its encryption keys after a membership change",
			wantHint: "re-keying (for 12s)", avoid: []string{"grant on the channel", "16m"},
		},
		{
			name: "rotating channel reported as unavailable by an older agent", args: publishArgs,
			fake: fakeChannel{status: http.StatusBadRequest, body: mlsUnavailable, provisioning: map[string]any{"phase": "active"},
				channel: map[string]any{"channel": map[string]any{"channel_id": "btr_channel_a", "state": "active", "rotation_required": true}}},
			wantKind: "channel_rekeying", wantMessage: "renewing its encryption keys", wantHint: "webhook was accepted or moved",
			avoid: []string{"missing the publish grant"},
		},
		{
			name: "epoch mismatch from an agent without the wait", args: publishArgs,
			fake:     fakeChannel{status: http.StatusBadRequest, body: daemonError("publish_failed", "btr mls: key epoch mismatch", nil)},
			wantKind: "channel_rekeying", wantMessage: "renewing its encryption keys", wantHint: "Retry in a moment",
		},
	})
}

// A bridge re-publishes another Room's message at its hop plus one.
func TestPublishSendsTheBridgeHop(t *testing.T) {
	var bodies []map[string]any
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"publication_id": "btr_pub_1"})
	}))
	run := func(args ...string) int {
		var stdout, stderr bytes.Buffer
		runner := Runner{Client: tunnel.NewLocalClient(socket), Out: &stdout, Err: &stderr}
		return runner.Run(context.Background(), append([]string{"tunnel", "room", "channel", "publish", "btr_room_a",
			"btr_channel_a", "--literal", "hi"}, args...))
	}
	if code := run("--hop", "2"); code != ExitOK || len(bodies) != 1 || bodies[0]["hop"] != float64(2) {
		t.Fatalf("code=%d bodies=%v", code, bodies)
	}
	if code := run(); code != ExitOK || len(bodies) != 2 {
		t.Fatalf("code=%d bodies=%v", code, bodies)
	}
	if _, ok := bodies[1]["hop"]; ok {
		t.Fatalf("hop sent without --hop: %v", bodies[1])
	}
	for _, args := range [][]string{{"--hop", "17"}, {"--hop", "1", "--retention", "sender_local"}} {
		if code := run(args...); code != ExitUsage || len(bodies) != 2 {
			t.Fatalf("args=%v code=%d bodies=%d", args, code, len(bodies))
		}
	}
}
