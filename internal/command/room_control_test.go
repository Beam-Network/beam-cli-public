package command

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func TestRoomControlSummaryNamesTheCauseAndSinceWhen(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 2, 30, 0, time.Local)
	since := now.Add(-150 * time.Second)
	for _, testCase := range []struct {
		control tunnel.RoomControl
		want    string
	}{
		{tunnel.RoomControl{State: tunnel.RoomControlHealthy, Since: since}, "healthy"},
		{tunnel.RoomControl{State: tunnel.RoomControlConnecting, Since: since}, "connecting to the coordinator"},
		{tunnel.RoomControl{State: tunnel.RoomControlDegraded, Since: since, Operation: tunnel.RoomControlDiscovery, Code: "unavailable"},
			"degraded since 12:00:00 (2m30s ago): Room discovery failed: no coordinator answered (unavailable)"},
		{tunnel.RoomControl{State: tunnel.RoomControlDegraded, Since: since, Operation: tunnel.RoomControlResume, RoomID: "btr_room_a", Code: "unavailable"},
			"degraded since 12:00:00 (2m30s ago): resuming the lease in Room btr_room_a failed: no coordinator answered (unavailable)"},
		{tunnel.RoomControl{State: tunnel.RoomControlDegraded, Since: since.Add(-24 * time.Hour), Operation: tunnel.RoomControlRenewal, RoomID: "btr_room_a", Code: "conflict"},
			"degraded since 2026-10-09 12:00:00 (24h2m30s ago): renewing the lease in Room btr_room_a failed: conflict"},
	} {
		if got := roomControlSummary(&testCase.control, now); got != testCase.want {
			t.Fatalf("summary = %q, want %q", got, testCase.want)
		}
	}
}

func roomFailure(roomID, operation, code string, since time.Time) tunnel.RoomControlFailure {
	return tunnel.RoomControlFailure{RoomID: roomID, Operation: operation, Code: code, Since: since}
}

func TestRoomControlGateBlocksOnlyWhatTheFailureReaches(t *testing.T) {
	now := time.Now()
	for _, control := range []*tunnel.RoomControl{nil, {State: tunnel.RoomControlHealthy}, {State: tunnel.RoomControlConnecting}} {
		if warning, err := roomControlGate(control, []string{"btr_room_a", "show"}, now); err != nil || warning != "" {
			t.Fatalf("%+v: warning=%q err=%v", control, warning, err)
		}
	}
	assertBlocked := func(control *tunnel.RoomControl, args []string, message string) {
		t.Helper()
		warning, err := roomControlGate(control, args, now)
		var cliErr *Error
		if warning != "" || !errors.As(err, &cliErr) || cliErr.Code != ExitOperationFailed ||
			cliErr.Kind != errorKindRoomControlDegraded || !strings.Contains(cliErr.Message, message) ||
			!strings.Contains(cliErr.Hint, "agent status") {
			t.Fatalf("%v: warning=%q err=%+v, want a block naming %q", args, warning, err, message)
		}
	}

	// A failed discovery fails every room: every command is blocked, even
	// while a room's lease failure is listed too.
	discovery := &tunnel.RoomControl{State: tunnel.RoomControlDegraded, Since: now.Add(-time.Minute),
		Operation: tunnel.RoomControlDiscovery, Code: "unavailable",
		FailingRooms: []tunnel.RoomControlFailure{roomFailure("btr_room_a", tunnel.RoomControlRenewal, "unavailable", now.Add(-2*time.Minute))}}
	for _, args := range [][]string{{"list"}, {"create"}, {"btr_room_b", "member", "list"}, {"inspect", "btr_room_a"}} {
		assertBlocked(discovery, args, "Room discovery failed: no coordinator answered (unavailable)")
	}

	// A room whose lease keeps failing blocks the commands that name it, with
	// its own failure, wherever the command spells the Room ID.
	rooms := &tunnel.RoomControl{State: tunnel.RoomControlDegraded, Since: now.Add(-30 * time.Second),
		Operation: tunnel.RoomControlRenewal, RoomID: "btr_room_a", Code: "conflict",
		FailingRooms: []tunnel.RoomControlFailure{
			roomFailure("btr_room_a", tunnel.RoomControlRenewal, "conflict", now.Add(-40*time.Second)),
			roomFailure("btr_room_c", tunnel.RoomControlResume, "unavailable", now.Add(-20*time.Second)),
		}}
	assertBlocked(rooms, []string{"btr_room_a", "member", "list"}, "degraded for Room btr_room_a since")
	assertBlocked(rooms, []string{"inspect", "btr_room_a"}, "renewing the lease in Room btr_room_a failed: conflict")
	assertBlocked(rooms, []string{"channel", "object", "publish", "btr_room_c", "btr_channel_a"},
		"resuming the lease in Room btr_room_c failed: no coordinator answered (unavailable)")

	// Commands for other Rooms, or for none, run with a warning.
	for _, args := range [][]string{{"btr_room_b", "member", "list"}, {"list"}, {"create"}} {
		warning, err := roomControlGate(rooms, args, now)
		if err != nil || !strings.HasPrefix(warning, "Warning: Room control is degraded since ") ||
			!strings.Contains(warning, "Commands for Rooms btr_room_a, btr_room_c fail until their leases recover") {
			t.Fatalf("%v: warning=%q err=%v", args, warning, err)
		}
	}
}
