package roomcli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	btr "github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btr"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/roommanager"
)

func TestParseInvitationChannelAccess(t *testing.T) {
	access, err := parseInvitationChannelAccess([]string{
		"channel-a=discover,publish",
		"channel-b=subscribe",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(access) != 2 || access[0].ChannelID != "channel-a" ||
		len(access[0].Actions) != 2 || access[0].Actions[1] != btr.Action("publish") {
		t.Fatalf("access=%#v", access)
	}
	if _, err := parseInvitationChannelAccess([]string{"channel-a=publish", "channel-a=subscribe"}); err == nil {
		t.Fatal("duplicate channel access accepted")
	}
}

func TestChannelInputAppliesStableDefaults(t *testing.T) {
	tests := []struct {
		kind            string
		ordering        btr.OrderingPolicy
		qos             btr.QoSClass
		reliability     btr.DeliveryReliability
		acknowledgement btr.AcknowledgementPolicy
		backpressure    btr.BackpressurePolicy
		maxPayload      uint64
	}{
		{"message", btr.OrderingPerPublisher, btr.QoSStandard, btr.DeliveryReliable, btr.AcknowledgementAccepted, btr.BackpressureDisconnect, btr.MaxMessageCiphertextBytes},
		{"stream", btr.OrderingPerFlow, btr.QoSInteractive, btr.DeliveryReliable, btr.AcknowledgementNone, btr.BackpressureDisconnect, btr.MaxStreamRecordBytes},
		{"datagram", btr.OrderingNone, btr.QoSInteractive, btr.DeliveryBestEffort, btr.AcknowledgementNone, btr.BackpressureDropOldest, 1024},
		{"request-reply", btr.OrderingPerPublisher, btr.QoSStandard, btr.DeliveryReliable, btr.AcknowledgementDelivered, btr.BackpressureDisconnect, btr.MaxMessageCiphertextBytes},
		{"media", btr.OrderingPerFlow, btr.QoSInteractive, btr.DeliveryBestEffort, btr.AcknowledgementNone, btr.BackpressureDropOldest, btr.MaxStreamRecordBytes},
		{"object", btr.OrderingPerFlow, btr.QoSBulk, btr.DeliveryReliable, btr.AcknowledgementDelivered, btr.BackpressureDisconnect, btr.MaxMessageCiphertextBytes},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			runner := Runner{Err: io.Discard}
			value, _, err := runner.channelInput([]string{"--name", "events", "--kind", test.kind}, false)
			if err != nil {
				t.Fatal(err)
			}
			input, ok := value.(roommanager.CreateChannelInput)
			if !ok {
				t.Fatalf("input type = %T", value)
			}
			if input.Ordering != test.ordering || input.QoS.Class != test.qos ||
				input.Delivery.Reliability != test.reliability || input.Delivery.Acknowledgement != test.acknowledgement ||
				input.Delivery.Backpressure != test.backpressure || input.Limits.MaxPayloadBytes != test.maxPayload {
				t.Fatalf("defaults = %#v", input.ChannelPolicyInput)
			}
		})
	}
}

func TestMediaPublishStatesTokenIsPerPublisherName(t *testing.T) {
	var out bytes.Buffer
	endpoint := mediaEndpoint{Name: "studio-cam", WHIPURL: "http://127.0.0.1/whip", Token: "token-a"}
	if err := (Runner{Out: &out}).renderMediaPublish(outputMode{}, endpoint, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "The token is valid only for publisher studio-cam.") {
		t.Fatalf("publish output = %q", out.String())
	}
}

func TestParsePersistenceModesRejectsDuplicates(t *testing.T) {
	if _, err := parsePersistenceModes("none,none"); err == nil {
		t.Fatal("expected duplicate mode error")
	}
	modes, err := parsePersistenceModes("none,sender_local")
	if err != nil {
		t.Fatal(err)
	}
	if len(modes) != 2 || modes[0] != btr.PersistenceNone || modes[1] != btr.PersistenceSenderLocal {
		t.Fatalf("modes = %#v", modes)
	}
}

func TestObjectSnapshotRendersTruthfulASCIIGrid(t *testing.T) {
	var output bytes.Buffer
	runner := Runner{Out: &output, Err: io.Discard}
	response := objectSnapshotFixture()
	if err := runner.renderObjectSnapshot(outputMode{}, response, "pub-1"); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, expected := range []string{
		"State     in_progress",
		"File      archive.tar.gz",
		"Chunks    4",
		"member-a                     [#.#.] 2/4 pending",
		"member-b                     [####] 4/4 delivered",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("output missing %q:\n%s", expected, got)
		}
	}
	if objectTerminal(response) {
		t.Fatal("in-progress transfer must not be terminal")
	}
	response["status"].(map[string]any)["publisher"].(map[string]any)["room_transfer"].(map[string]any)["status"] = "completed"
	if !objectTerminal(response) {
		t.Fatal("completed transfer must be terminal")
	}
}

func TestFollowedObjectPublicationReturnsTerminalOutcome(t *testing.T) {
	for _, test := range []struct {
		name         string
		state        string
		allowPartial bool
		json         bool
		failed       bool
	}{
		{"completed", "completed", false, false, false},
		{"strict_partial", "partial", false, false, true},
		{"allowed_partial", "partial", true, false, false},
		{"failed_json", "failed", false, true, true},
		{"cancelled", "cancelled", false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			runner := Runner{Out: &output, Err: io.Discard}
			response := map[string]any{"status": map[string]any{"publisher": map[string]any{
				"state": test.state, "room_transfer": map[string]any{"status": test.state},
				"deliveries": []any{map[string]any{"member_id": "member-b", "state": "failed"}},
			}}}
			err := runner.followObject(context.Background(), outputMode{JSON: test.json}, "", "pub", time.Second,
				response, true, test.allowPartial)
			if (err != nil) != test.failed || !strings.Contains(output.String(), test.state) {
				t.Fatalf("err=%v output=%q", err, output.String())
			}
			if err := runner.followObject(context.Background(), outputMode{}, "", "pub", time.Second,
				response, false, false); err != nil {
				t.Fatalf("status inspection failed: %v", err)
			}
		})
	}
}

func objectSnapshotFixture() map[string]any {
	return map[string]any{
		"status": map[string]any{
			"publisher": map[string]any{
				"state": "active",
				"room_transfer": map[string]any{
					"status":   "in_progress",
					"filename": "archive.tar.gz",
					"file":     map[string]any{"chunk_count": float64(4), "size_bytes": float64(2048)},
				},
				"deliveries": []any{
					map[string]any{"member_id": "member-a", "state": "pending", "completed_chunks": float64(2), "coverage_base64": "BQ=="},
					map[string]any{"member_id": "member-b", "state": "delivered", "completed_chunks": float64(4), "coverage_base64": "Dw=="},
				},
			},
		},
	}
}

func TestRoomStateSelected(t *testing.T) {
	for _, test := range []struct {
		name      string
		roomState string
		filter    string
		selected  bool
	}{
		// A closed room is retained by the daemon, so the default listing must
		// hide it rather than report a room that no longer exists.
		{name: "default hides closed", roomState: "closed", filter: "", selected: false},
		{name: "default keeps active", roomState: "active", filter: "", selected: true},
		// Suspended is not terminal; hiding it would conceal a live room.
		{name: "default keeps suspended", roomState: "suspended", filter: "", selected: true},
		{name: "all keeps closed", roomState: "closed", filter: "all", selected: true},
		{name: "all keeps active", roomState: "active", filter: "all", selected: true},
		{name: "exact match", roomState: "closed", filter: "closed", selected: true},
		{name: "exact mismatch", roomState: "active", filter: "closed", selected: false},
		{name: "case insensitive", roomState: "CLOSED", filter: "Closed", selected: true},
		{name: "unknown state kept by default", roomState: "draining", filter: "", selected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := roomStateSelected(test.roomState, test.filter); got != test.selected {
				t.Errorf("roomStateSelected(%q, %q) = %t, want %t", test.roomState, test.filter, got, test.selected)
			}
		})
	}
}

func TestEmptyListNotice(t *testing.T) {
	for _, test := range []struct {
		name     string
		hidden   int
		filter   string
		contains []string
	}{
		// Nothing was fetched, so there is no filter to relax. The note points at
		// the two commands that produce a first room instead.
		{
			name:     "no rooms at all",
			hidden:   0,
			filter:   "",
			contains: []string{"no rooms.", "beam room create --api-key", "beam room join ROOM_ID"},
		},
		// The default filter hides exactly the closed rooms, so the count names
		// them rather than describing the filter in the abstract.
		{
			name:     "closed rooms hidden by default",
			hidden:   3,
			filter:   "",
			contains: []string{"no open rooms (3 closed rooms hidden).", `"beam room list --state all"`},
		},
		{
			name:     "single closed room reads as singular",
			hidden:   1,
			filter:   "",
			contains: []string{"no open rooms (1 closed room hidden)."},
		},
		// An explicit filter is echoed back, which is what turns a mistyped state
		// from a blank screen into a visible cause.
		{
			name:     "explicit filter is echoed",
			hidden:   2,
			filter:   "activ",
			contains: []string{`no rooms in state "activ" (2 rooms hidden).`, `"beam room list --state all"`},
		},
		{
			name:     "explicit filter is normalized",
			hidden:   1,
			filter:   "  CLOSED  ",
			contains: []string{`no rooms in state "closed" (1 room hidden).`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			Runner{Out: io.Discard, Err: &stderr}.emptyListNotice(test.hidden, test.filter)
			got := stderr.String()
			for _, expected := range test.contains {
				if !strings.Contains(got, expected) {
					t.Errorf("notice %q does not contain %q", got, expected)
				}
			}
		})
	}
}
