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
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// roomControlDaemon serves a running agent whose Room control is control. Set
// replaces it.
type roomControlDaemon struct {
	mu      sync.Mutex
	control *tunnel.RoomControl
}

func (daemon *roomControlDaemon) set(control *tunnel.RoomControl) {
	daemon.mu.Lock()
	daemon.control = control
	daemon.mu.Unlock()
}

func (daemon *roomControlDaemon) status() tunnel.Status {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	return tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion,
		RoomControl: daemon.control}
}

func startRoomControlDaemon(t *testing.T, control *tunnel.RoomControl) *roomControlDaemon {
	t.Helper()
	daemon := &roomControlDaemon{control: control}
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(daemon.status())
		case r.Method == http.MethodGet && r.URL.Path == tunnel.AgentConnectionPath:
			_ = json.NewEncoder(w).Encode(tunnel.AgentConnectionStatus{Prepared: true, Registered: true, Connected: true,
				State: "connected", AgentID: "agt_test", Label: "builder", CoordinatorURL: "https://coordinator.example.com"})
		case r.Method == http.MethodGet && r.URL.Path == tunnel.MetricsPath:
			_ = json.NewEncoder(w).Encode(tunnel.Metrics{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/rooms/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"memberships": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	testutil.IsolateAgent(t)
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	return daemon
}

func degradedDiscovery() *tunnel.RoomControl {
	return &tunnel.RoomControl{State: tunnel.RoomControlDegraded, Since: time.Now().Add(-time.Minute),
		CheckedAt: time.Now(), Operation: tunnel.RoomControlDiscovery, Code: "unavailable"}
}

func TestAgentStatusReportsDegradedRoomControl(t *testing.T) {
	startRoomControlDaemon(t, degradedDiscovery())
	app := New(version.BuildInfo{Version: "test"})
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"agent", "status"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	want := "Room control: degraded since "
	if !strings.Contains(stdout.String(), "agt_test\tbuilder\tconnected") || !strings.Contains(stdout.String(), want) ||
		!strings.Contains(stdout.String(), "Room discovery failed: no coordinator answered (unavailable)") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	stdout.Reset()
	if code := app.Run(context.Background(), []string{"agent", "status", "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var report struct {
		AgentID     string              `json:"agent_id"`
		RoomControl *tunnel.RoomControl `json:"room_control"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.AgentID != "agt_test" || !report.RoomControl.Degraded() || report.RoomControl.Operation != tunnel.RoomControlDiscovery ||
		report.RoomControl.Code != "unavailable" {
		t.Fatalf("report = %+v room_control=%+v", report, report.RoomControl)
	}
}

func TestBeamStatusReportsRoomControl(t *testing.T) {
	daemon := startRoomControlDaemon(t, degradedDiscovery())
	app := New(version.BuildInfo{Version: "test"})
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"status"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Room control: degraded since ") ||
		!strings.Contains(stdout.String(), "Room discovery failed: no coordinator answered (unavailable)") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	daemon.set(&tunnel.RoomControl{State: tunnel.RoomControlHealthy, Since: time.Now()})
	stdout.Reset()
	if code := app.Run(context.Background(), []string{"status"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Room control: healthy\n") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

// `agent start` waits for the agent's first room discovery and warns when
// Room control came up degraded.
func TestAgentStartWarnsWhenRoomControlIsDegraded(t *testing.T) {
	daemon := startRoomControlDaemon(t, degradedDiscovery())
	app := New(version.BuildInfo{Version: "test"})
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		return tunnel.StartResult{PID: 42, Status: tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion,
			ProtocolMinVersion: tunnel.ProtocolMinVersion,
			RoomControl:        &tunnel.RoomControl{State: tunnel.RoomControlConnecting, Since: time.Now()}}}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"agent", "start"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "started") ||
		!strings.Contains(stderr.String(), "Warning: Room control is degraded: Room discovery failed: no coordinator answered (unavailable).") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run(context.Background(), []string{"agent", "start", "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var result tunnel.StartResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Status.RoomControl.Degraded() || stderr.Len() != 0 {
		t.Fatalf("result=%+v stderr=%q", result, stderr.String())
	}

	// A healthy start says nothing about Room control.
	daemon.set(&tunnel.RoomControl{State: tunnel.RoomControlHealthy, Since: time.Now()})
	stdout.Reset()
	stderr.Reset()
	if code := app.Run(context.Background(), []string{"agent", "start"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestAgentStartReportsRoomControlStillConnecting(t *testing.T) {
	previous := roomControlSettleTimeout
	roomControlSettleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { roomControlSettleTimeout = previous })
	connecting := &tunnel.RoomControl{State: tunnel.RoomControlConnecting, Since: time.Now()}
	startRoomControlDaemon(t, connecting)
	app := New(version.BuildInfo{Version: "test"})
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		return tunnel.StartResult{PID: 42, Status: tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion,
			ProtocolMinVersion: tunnel.ProtocolMinVersion, RoomControl: connecting}}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"agent", "start"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Room control is still connecting to the coordinator") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

// Room commands fail with the cause while Room control is degraded, and run
// once it recovers.
func TestRoomCommandsFailWhileRoomControlIsDegraded(t *testing.T) {
	daemon := startRoomControlDaemon(t, degradedDiscovery())
	app := New(version.BuildInfo{Version: "test"})
	runs := 0
	app.roomRun = func(context.Context, []string, string, string, output.Renderer) error {
		runs++
		return nil
	}
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		t.Fatal("a running agent must not be started")
		return tunnel.StartResult{}, errors.New("unreachable")
	}
	for _, args := range [][]string{{"room", "list", "--json"}, {"room", "btr_room_a", "member", "list", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := app.Run(context.Background(), args, &stdout, &stderr); code != ExitOperationFailed {
			t.Fatalf("%v: code=%d stderr=%s", args, code, stderr.String())
		}
		var payload struct {
			Error struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
			t.Fatalf("%v: stderr=%q err=%v", args, stderr.String(), err)
		}
		if payload.Error.Kind != errorKindRoomControlDegraded ||
			!strings.Contains(payload.Error.Message, "Room discovery failed: no coordinator answered (unavailable)") {
			t.Fatalf("%v: error = %+v", args, payload.Error)
		}
	}
	if runs != 0 {
		t.Fatalf("room command ran %d times while Room control was degraded", runs)
	}
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"room", "help"}, &stdout, &stderr); code != ExitOK || runs != 1 {
		t.Fatalf("room help: code=%d runs=%d stderr=%s", code, runs, stderr.String())
	}

	daemon.set(&tunnel.RoomControl{State: tunnel.RoomControlHealthy, Since: time.Now()})
	if code := app.Run(context.Background(), []string{"room", "list"}, &stdout, &stderr); code != ExitOK || runs != 2 {
		t.Fatalf("room list: code=%d runs=%d stderr=%s", code, runs, stderr.String())
	}
}

// A room whose lease keeps failing blocks only the commands for that room;
// the others run, with the warning on the status stream only.
func TestRoomCommandsForOtherRoomsRunWhileOneRoomFails(t *testing.T) {
	since := time.Now().Add(-30 * time.Second)
	startRoomControlDaemon(t, &tunnel.RoomControl{State: tunnel.RoomControlDegraded, Since: since,
		Operation: tunnel.RoomControlRenewal, RoomID: "btr_room_a", Code: "unavailable",
		FailingRooms: []tunnel.RoomControlFailure{{RoomID: "btr_room_a", Operation: tunnel.RoomControlRenewal,
			Code: "unavailable", Since: since}}})
	app := New(version.BuildInfo{Version: "test"})
	runs := 0
	app.roomRun = func(_ context.Context, _ []string, _, _ string, renderer output.Renderer) error {
		runs++
		return renderer.Result(map[string]any{"rooms": []any{}}, nil)
	}
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		t.Fatal("a running agent must not be started")
		return tunnel.StartResult{}, errors.New("unreachable")
	}
	for _, args := range [][]string{{"room", "btr_room_a", "member", "list", "--json"}, {"room", "inspect", "btr_room_a", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := app.Run(context.Background(), args, &stdout, &stderr); code != ExitOperationFailed {
			t.Fatalf("%v: code=%d stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), `"kind":"room_control_degraded"`) ||
			!strings.Contains(stderr.String(), "renewing the lease in Room btr_room_a failed: no coordinator answered (unavailable)") {
			t.Fatalf("%v: stderr=%q", args, stderr.String())
		}
	}
	if runs != 0 {
		t.Fatalf("a command for the failing Room ran %d times", runs)
	}

	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"room", "list"}, &stdout, &stderr); code != ExitOK || runs != 1 {
		t.Fatalf("room list: code=%d runs=%d stderr=%s", code, runs, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Warning: Room control is degraded since ") ||
		!strings.Contains(stderr.String(), "Commands for Room btr_room_a fail until its lease recovers") {
		t.Fatalf("room list: stderr=%q", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run(context.Background(), []string{"room", "list", "--json"}, &stdout, &stderr); code != ExitOK || runs != 2 {
		t.Fatalf("room list --json: code=%d runs=%d stderr=%s", code, runs, stderr.String())
	}
	if stderr.Len() != 0 || strings.Contains(stdout.String(), "Warning") {
		t.Fatalf("room list --json: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
