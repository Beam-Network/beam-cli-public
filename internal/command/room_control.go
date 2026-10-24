package command

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

const errorKindRoomControlDegraded = "room_control_degraded"

// roomControlSettleTimeout bounds how long `beam agent start` waits for the
// agent's first room discovery, so it reports the Room control the agent ends
// up with rather than "connecting". A reachable coordinator answers within a
// second; an unreachable one fails discovery as fast as the connection fails.
var roomControlSettleTimeout = 10 * time.Second

const roomControlSettleInterval = 200 * time.Millisecond

type statusClient interface {
	Status(context.Context) (tunnel.Status, error)
}

// settleRoomControl returns status once the agent's Room control has left
// connecting, or the latest status when roomControlSettleTimeout passes first.
func settleRoomControl(ctx context.Context, client statusClient, status tunnel.Status) tunnel.Status {
	if status.RoomControl == nil || status.RoomControl.State != tunnel.RoomControlConnecting {
		return status
	}
	deadline := time.NewTimer(roomControlSettleTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(roomControlSettleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status
		case <-deadline.C:
			return status
		case <-ticker.C:
			latest, err := client.Status(ctx)
			if err != nil {
				continue
			}
			status = latest
			if status.RoomControl == nil || status.RoomControl.State != tunnel.RoomControlConnecting {
				return status
			}
		}
	}
}

// roomControlCause says which operation failed and why.
func roomControlCause(operation, roomID, code string) string {
	failed := "Room discovery failed"
	switch operation {
	case tunnel.RoomControlResume:
		failed = fmt.Sprintf("resuming the lease in Room %s failed", roomID)
	case tunnel.RoomControlRenewal:
		failed = fmt.Sprintf("renewing the lease in Room %s failed", roomID)
	}
	if code == "unavailable" {
		return failed + ": no coordinator answered (unavailable)"
	}
	return failed + ": " + code
}

// roomControlSince says since when, and for how long.
func roomControlSince(since, now time.Time) string {
	local := since.Local()
	layout := "15:04:05"
	if !sameDay(local, now.Local()) {
		layout = "2006-01-02 15:04:05"
	}
	return fmt.Sprintf("since %s (%s ago)", local.Format(layout), now.Sub(since).Round(time.Second))
}

func sameDay(left, right time.Time) bool {
	leftYear, leftMonth, leftDay := left.Date()
	rightYear, rightMonth, rightDay := right.Date()
	return leftYear == rightYear && leftMonth == rightMonth && leftDay == rightDay
}

// roomControlSummary is the Room control line of a status report.
func roomControlSummary(control *tunnel.RoomControl, now time.Time) string {
	switch control.State {
	case tunnel.RoomControlHealthy:
		return "healthy"
	case tunnel.RoomControlConnecting:
		return "connecting to the coordinator"
	case tunnel.RoomControlDegraded:
		return fmt.Sprintf("degraded %s: %s", roomControlSince(control.Since, now),
			roomControlCause(control.Operation, control.RoomID, control.Code))
	default:
		return control.State
	}
}

// roomControlGate decides whether a room command with args may run while the
// agent reports control. A failed discovery fails every room, so it blocks
// every command; a room whose lease keeps failing blocks only the commands
// that name it. A command for other rooms runs, and warning says why it may
// still be affected.
func roomControlGate(control *tunnel.RoomControl, args []string, now time.Time) (warning string, err error) {
	if !control.Degraded() {
		return "", nil
	}
	hint := applyReleaseNames("Check network access to the coordinator; `beam agent status` shows when Room control recovers and `beam agent logs` each failed attempt.")
	if control.Operation == tunnel.RoomControlDiscovery {
		return "", typedCLIError(ExitOperationFailed, errorKindRoomControlDegraded,
			fmt.Sprintf("Room control is degraded %s: %s. Room commands fail until the agent reaches the coordinator again.",
				roomControlSince(control.Since, now), roomControlCause(control.Operation, control.RoomID, control.Code)),
			hint, nil)
	}
	for _, failure := range control.FailingRooms {
		if slices.Contains(args, failure.RoomID) {
			return "", typedCLIError(ExitOperationFailed, errorKindRoomControlDegraded,
				fmt.Sprintf("Room control is degraded for Room %s %s: %s. Commands for this Room fail until its lease recovers.",
					failure.RoomID, roomControlSince(failure.Since, now), roomControlCause(failure.Operation, failure.RoomID, failure.Code)),
				hint, nil)
		}
	}
	return applyReleaseNames(fmt.Sprintf("Warning: Room control is degraded %s: %s. %s; `beam agent status` shows when it recovers.",
		roomControlSince(control.Since, now), roomControlCause(control.Operation, control.RoomID, control.Code),
		roomControlImpact(control))), nil
}

// roomControlImpact says which room commands fail while control is degraded.
func roomControlImpact(control *tunnel.RoomControl) string {
	if control.Operation == tunnel.RoomControlDiscovery {
		return "Rooms do not work until the agent reaches the coordinator"
	}
	rooms := make([]string, 0, len(control.FailingRooms))
	for _, failure := range control.FailingRooms {
		rooms = append(rooms, failure.RoomID)
	}
	if len(rooms) == 1 {
		return "Commands for Room " + rooms[0] + " fail until its lease recovers"
	}
	return "Commands for Rooms " + strings.Join(rooms, ", ") + " fail until their leases recover"
}

// requireRoomControl applies roomControlGate to a room command with args: it
// fails the command, or warns on the status stream and lets it run. A stopped
// or unanswering agent is left to the command, which starts it or reports the
// failure.
func requireRoomControl(ctx context.Context, client statusClient, args []string, renderer output.Renderer) error {
	status, err := client.Status(ctx)
	if err != nil {
		return nil
	}
	warning, blocked := roomControlGate(status.RoomControl, args, time.Now())
	if blocked != nil {
		return blocked
	}
	if warning != "" {
		renderer.Progress(warning)
	}
	return nil
}

// warnRoomControl tells a person who just started the agent that its Room
// control is degraded or still connecting.
func warnRoomControl(control *tunnel.RoomControl, renderer output.Renderer) {
	switch {
	case control.Degraded():
		renderer.Progress(applyReleaseNames(fmt.Sprintf("Warning: Room control is degraded: %s. %s; `beam agent status` shows when it recovers.",
			roomControlCause(control.Operation, control.RoomID, control.Code), roomControlImpact(control))))
	case control != nil && control.State == tunnel.RoomControlConnecting:
		renderer.Progress(applyReleaseNames("Room control is still connecting to the coordinator; `beam agent status` shows when it is ready."))
	}
}
