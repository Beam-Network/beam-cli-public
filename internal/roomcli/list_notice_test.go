//go:build !windows

package roomcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/state"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

// listRoomsFixture is the daemon payload for one room in the given state.
const listRoomsFixture = `{"rooms":[{"room":{"room_id":"btr_room_a","organization_id":"org_a","state":%q},` +
	`"membership":{"member_id":"btr_member_a"},"authorization_epoch":1,"resume":{"notification_cursor":7}}]}`

func listRunner(t *testing.T, payload string) (Runner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/rooms" {
			http.NotFound(w, request)
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	var stdout, stderr bytes.Buffer
	return Runner{Client: tunnel.NewLocalClient(socket), Out: &stdout, Err: &stderr}, &stdout, &stderr
}

// TestListEmptyNoticeIsHumanOnly pins the contract that makes the notice safe:
// it explains a blank listing to a person without changing what a script reads.
func TestListEmptyNoticeIsHumanOnly(t *testing.T) {
	for _, test := range []struct {
		name           string
		args           []string
		stdout         string
		stderrContains string
		stderrEmpty    bool
	}{
		{
			name:           "human listing explains the hidden room",
			args:           []string{"tunnel", "room", "list"},
			stdout:         "",
			stderrContains: "no open rooms (1 closed room hidden).",
		},
		// --json is consumed by programs, which must keep seeing an empty array
		// rather than a sentence on the side channel.
		{
			name:        "json stays a bare empty array",
			args:        []string{"tunnel", "room", "--json", "list"},
			stdout:      "{\"rooms\":[]}\n",
			stderrEmpty: true,
		},
		// --quiet exists so a shell can consume bare IDs. A notice would be noise
		// in exactly the pipelines that asked for silence.
		{
			name:        "quiet stays silent on both streams",
			args:        []string{"tunnel", "room", "--quiet", "list"},
			stdout:      "",
			stderrEmpty: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, stdout, stderr := listRunner(t, strings.Replace(listRoomsFixture, "%q", `"closed"`, 1))
			if code := runner.Run(context.Background(), test.args); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if stdout.String() != test.stdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), test.stdout)
			}
			if test.stderrEmpty && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
			if test.stderrContains != "" && !strings.Contains(stderr.String(), test.stderrContains) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), test.stderrContains)
			}
		})
	}
}

// TestListPrintsNoNoticeWhenRoomsAreShown guards the regression that matters
// most: an ordinary listing must not gain an extra line.
func TestListPrintsNoNoticeWhenRoomsAreShown(t *testing.T) {
	runner, stdout, stderr := listRunner(t, strings.Replace(listRoomsFixture, "%q", `"active"`, 1))
	if code := runner.Run(context.Background(), []string{"tunnel", "room", "list"}); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	want := strings.Join([]string{
		"ROOM ID     STATE   ORGANIZATION  MEMBER",
		"btr_room_a  active  org_a         btr_member_a",
		"",
	}, "\n")
	if stdout.String() != want {
		t.Errorf("stdout =\n%q\nwant\n%q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestListNoticeWhenNoRoomsExist covers the other blank listing: nothing was
// fetched, so no filter would reveal anything and the note names create/join.
func TestListNoticeWhenNoRoomsExist(t *testing.T) {
	runner, stdout, stderr := listRunner(t, `{"rooms":[]}`)
	if code := runner.Run(context.Background(), []string{"tunnel", "room", "list"}); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	for _, expected := range []string{"no rooms.", "beam room create --api-key", "beam room join ROOM_ID"} {
		if !strings.Contains(stderr.String(), expected) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), expected)
		}
	}
}

// TestListNoticeEchoesUnmatchedFilter is the mistyped --state case: the room is
// active, the filter is not, and the cause has to be visible.
func TestListNoticeEchoesUnmatchedFilter(t *testing.T) {
	runner, stdout, stderr := listRunner(t, strings.Replace(listRoomsFixture, "%q", `"active"`, 1))
	if code := runner.Run(context.Background(), []string{"tunnel", "room", "list", "--state", "activ"}); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), `no rooms in state "activ" (1 room hidden).`) {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// Dropping the epoch and cursor columns is a change to the human listing only.
// A caller that needs them asks for --json, and "room <id> show" still prints
// them, so this pins that the data itself was not removed.
func TestListJSONStillCarriesEpochAndCursor(t *testing.T) {
	runner, stdout, stderr := listRunner(t, strings.Replace(listRoomsFixture, "%q", `"active"`, 1))
	if code := runner.Run(context.Background(), []string{"tunnel", "room", "--json", "list"}); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var payload struct {
		Rooms []struct {
			AuthorizationEpoch uint64 `json:"authorization_epoch"`
			Resume             struct {
				NotificationCursor uint64 `json:"notification_cursor"`
			} `json:"resume"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.Rooms) != 1 {
		t.Fatalf("rooms = %d, want 1", len(payload.Rooms))
	}
	if payload.Rooms[0].AuthorizationEpoch != 1 {
		t.Errorf("authorization_epoch = %d, want 1", payload.Rooms[0].AuthorizationEpoch)
	}
	if payload.Rooms[0].Resume.NotificationCursor != 7 {
		t.Errorf("notification_cursor = %d, want 7", payload.Rooms[0].Resume.NotificationCursor)
	}
}

// "room <id> show" is where the two values moved to, so it must keep printing
// them under their full names.
func TestRenderRoomStillShowsEpochAndCursor(t *testing.T) {
	var stdout bytes.Buffer
	runner := Runner{Out: &stdout, Err: io.Discard}
	record := state.BTRRoomRecord{AuthorizationEpoch: 4}
	record.Room.RoomID = "btr_room_a"
	record.Resume.NotificationCursor = 9
	if err := runner.renderRoom(outputMode{}, record); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Authorization epoch: 4", "Notification cursor: 9"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("room show output %q is missing %q", stdout.String(), want)
		}
	}
}
