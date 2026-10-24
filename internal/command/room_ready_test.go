//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// roomReadyApp runs room commands through results, one per run, and starts
// the agent with start. It reports how many times each was called.
func roomReadyApp(t *testing.T, start tunnel.StartResult, results ...error) (*App, func() (int, int)) {
	t.Helper()
	testutil.IsolateAgent(t)
	runs, starts := 0, 0
	app := New(version.BuildInfo{Version: "test"})
	app.roomRun = func(context.Context, []string, string, string, output.Renderer) error {
		runs++
		if runs > len(results) {
			t.Fatalf("room command ran %d times, want at most %d", runs, len(results))
		}
		return results[runs-1]
	}
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		starts++
		return start, nil
	}
	return app, func() (int, int) { return runs, starts }
}

// A room command that failed after reaching the agent, such as a followed
// publication that ended partial, is never run again and starts nothing.
func TestRunRoomReadyDoesNotRunAgainAfterAFailureThatReachedTheAgent(t *testing.T) {
	failure := typedCLIError(ExitOperationFailed, "object_publication_partial", "Room object publication btr_pub_1 was only partially delivered.", "", nil)
	app, calls := roomReadyApp(t, tunnel.StartResult{}, failure)
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"room", "list"}, &stdout, &stderr); code != ExitOperationFailed {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if runs, starts := calls(); runs != 1 || starts != 0 {
		t.Fatalf("runs=%d starts=%d, want one run and no agent start", runs, starts)
	}
}

// A room command that could not reach the agent starts it and runs again.
func TestRunRoomReadyStartsTheAgentAndRunsAgain(t *testing.T) {
	unavailable := cliError(ExitDaemonUnavailable, "beam-agentd is unavailable.", "", nil)
	app, calls := roomReadyApp(t, tunnel.StartResult{PID: 42}, unavailable, nil)
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"room", "list"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if runs, starts := calls(); runs != 2 || starts != 1 {
		t.Fatalf("runs=%d starts=%d, want two runs around one agent start", runs, starts)
	}
}

// When the agent turns out to be running, the command may have reached it
// before failing, so it is not run again.
func TestRunRoomReadyDoesNotRunAgainWhenTheAgentWasRunning(t *testing.T) {
	unavailable := cliError(ExitDaemonUnavailable, "beam-agentd is unavailable.", "", nil)
	app, calls := roomReadyApp(t, tunnel.StartResult{AlreadyRunning: true}, unavailable)
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"room", "list"}, &stdout, &stderr); code != ExitDaemonUnavailable {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if runs, starts := calls(); runs != 1 || starts != 1 {
		t.Fatalf("runs=%d starts=%d, want one run and one start check", runs, starts)
	}
}

// A room request interrupted after it connected to the agent may have reached
// it, so it is an operation failure that is not run again.
func TestRunRoomReadyDoesNotRunAgainAfterAnInterruptedRequest(t *testing.T) {
	app, calls := roomReadyApp(t, tunnel.StartResult{}, mapTunnelError(&tunnel.Error{Kind: tunnel.ErrInterrupted}))
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"room", "list"}, &stdout, &stderr); code != ExitOperationFailed {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if runs, starts := calls(); runs != 1 || starts != 0 {
		t.Fatalf("runs=%d starts=%d, want one run and no agent start", runs, starts)
	}
}

// An object publish whose connection the agent closed after reading it is
// sent once, and the error says how to find the publication it may have made.
func TestInterruptedObjectPublishIsNotPublishedAgain(t *testing.T) {
	const channelPath = "/v1/rooms/btr_room_a/channels/btr_channel_files"
	var mu sync.Mutex
	publishes := 0
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case r.Method == http.MethodGet && r.URL.Path == channelPath:
			_, _ = w.Write([]byte(`{"channel":{"channel_id":"btr_channel_files","room_id":"btr_room_a","kind":"object","limits":{"publication_ttl":60000000000}}}`))
		case r.Method == http.MethodPost && r.URL.Path == channelPath+"/objects/publish":
			mu.Lock()
			publishes++
			mu.Unlock()
			if connection, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = connection.Close()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	testutil.IsolateAgent(t)
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	app := New(version.BuildInfo{Version: "test"})
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		t.Fatal("a running agent must not be started")
		return tunnel.StartResult{}, errors.New("unreachable")
	}
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"room", "btr_room_a", "channel", "btr_channel_files", "object", "publish", "--file", "./f10k.bin", "--follow", "--json"}, &stdout, &stderr)
	if code != ExitOperationFailed {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	mu.Lock()
	if publishes != 1 {
		t.Fatalf("publishes=%d, want 1", publishes)
	}
	mu.Unlock()
	var payload struct {
		Error struct {
			Kind string `json:"kind"`
			Hint string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr=%q err=%v", stderr.String(), err)
	}
	if payload.Error.Kind != "agent_request_interrupted" || !strings.Contains(payload.Error.Hint, "object list") {
		t.Fatalf("error=%+v", payload.Error)
	}
}

// A followed object publish that ends partial or failed against a running
// agent publishes once and exits with an operation failure naming the outcome
// and the recipients that missed the file, with their reasons.
func TestFollowedObjectPublishFailureIsNotPublishedAgain(t *testing.T) {
	const (
		channelPath = "/v1/rooms/btr_room_a/channels/btr_channel_files"
		publishPath = channelPath + "/objects/publish"
	)
	for _, test := range []struct {
		name     string
		state    string
		flags    []string
		wantCode int
		wantKind string
		wantText string
	}{
		{name: "partial", state: "partial", wantCode: ExitOperationFailed, wantKind: "object_publication_partial",
			wantText: "was only partially delivered: member btr_member_b unavailable. It failed for 1 of 2 recipients: btr_member_b (agent_offline)."},
		{name: "failed", state: "failed", wantCode: ExitOperationFailed, wantKind: "object_publication_failed",
			wantText: "failed: member btr_member_b unavailable. It failed for 1 of 2 recipients: btr_member_b (agent_offline)."},
		{name: "partial with --allow-partial", state: "partial", flags: []string{"--allow-partial"}, wantCode: ExitOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			publishes := 0
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == tunnel.StatusPath:
					_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
				case r.Method == http.MethodGet && r.URL.Path == channelPath:
					_, _ = w.Write([]byte(`{"channel":{"channel_id":"btr_channel_files","room_id":"btr_room_a","kind":"object","limits":{"publication_ttl":60000000000}}}`))
				case r.Method == http.MethodPost && r.URL.Path == publishPath:
					mu.Lock()
					publishes++
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]any{"publication_id": "btr_pub_1", "status": map[string]any{"publisher": map[string]any{
						"state": test.state, "room_transfer": map[string]any{"status": test.state, "error_message": "member btr_member_b unavailable"},
						"deliveries": []any{
							map[string]any{"member_id": "btr_member_a", "state": "delivered"},
							map[string]any{"member_id": "btr_member_b", "state": "failed", "unavailable_reason": "agent_offline"},
						},
					}}})
				default:
					http.NotFound(w, r)
				}
			}))
			testutil.IsolateAgent(t)
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			app := New(version.BuildInfo{Version: "test"})
			app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
				t.Fatal("a running agent must not be started")
				return tunnel.StartResult{}, errors.New("unreachable")
			}
			args := append([]string{"room", "btr_room_a", "channel", "btr_channel_files", "object", "publish", "--file", "./f10k.bin", "--follow", "--json"}, test.flags...)
			var stdout, stderr bytes.Buffer
			code := app.Run(context.Background(), args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("code=%d want=%d stderr=%s", code, test.wantCode, stderr.String())
			}
			mu.Lock()
			if publishes != 1 {
				t.Fatalf("publishes=%d, want 1", publishes)
			}
			mu.Unlock()
			if test.wantCode == ExitOK {
				return
			}
			var payload struct {
				Error struct {
					Code    int    `json:"code"`
					Kind    string `json:"kind"`
					Message string `json:"message"`
					Hint    string `json:"hint"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
				t.Fatalf("stderr=%q err=%v", stderr.String(), err)
			}
			if payload.Error.Code != test.wantCode || payload.Error.Kind != test.wantKind || !strings.Contains(payload.Error.Message, test.wantText) {
				t.Fatalf("error=%+v", payload.Error)
			}
			if strings.Contains(stderr.String(), "starts the agent") || strings.Contains(stderr.String(), "Retry the command") {
				t.Fatalf("error suggests starting the agent: %s", stderr.String())
			}
		})
	}
}
