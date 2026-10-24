package roomcli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestObjectStatusDisplaysFrozenProtectionAndRuntimeIdentity(t *testing.T) {
	for _, tc := range []struct{ scheme, expected string }{
		{"btr.object.chunk.aead.v1", "Room MLS E2EE"},
		{"btr.object.transport.tls.v1", "TLS; workers handle plaintext for every recipient"},
	} {
		var output bytes.Buffer
		runner := Runner{Out: &output}
		err := runner.renderObjectSnapshot(outputMode{}, map[string]any{"status": map[string]any{"publisher": map[string]any{"room_transfer": map[string]any{
			"status": "completed", "transfer_id": "runtime-identity", "protection": map[string]any{"scheme": tc.scheme},
			"file": map[string]any{"size_bytes": float64(104857600), "chunk_size_bytes": float64(41943040), "chunk_count": float64(3)},
		}}}}, "publication")
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{"Runtime   runtime-identity", tc.expected, "Chunk size"} {
			if !strings.Contains(output.String(), expected) {
				t.Fatalf("missing %q in %s", expected, output.String())
			}
		}
	}
}

const destinationAccessDenied = "destination_access_denied: The destination storage refused Beam's requests (403 AccessDenied). Check that the credentials allow writes to this bucket and path and are not restricted to specific IP addresses or networks."

func objectStatusWithReason(state, message string) map[string]any {
	transfer := map[string]any{"status": state}
	if message != "" {
		transfer["error_code"] = "destination_access_denied"
		transfer["error_message"] = message
	}
	return map[string]any{"status": map[string]any{"publisher": map[string]any{
		"state": state, "room_transfer": transfer,
	}}}
}

func TestObjectStatusShowsTheWholeFailureReason(t *testing.T) {
	if len([]rune(destinationAccessDenied)) <= maxFailureReasonLength {
		t.Fatal("the reason must be longer than a shortened daemon failure reason")
	}
	var output bytes.Buffer
	runner := Runner{Out: &output}
	if err := runner.renderObjectSnapshot(outputMode{}, objectStatusWithReason("failed", destinationAccessDenied+"\r\n"), "publication"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "State     failed\nError     "+destinationAccessDenied+"\n") {
		t.Fatalf("output=%q", output.String())
	}
}

var testPublication = objectPublication{RoomID: "btr_room_a", ChannelID: "btr_channel_files", PublicationID: "btr_pub_1"}

func TestFollowedPublicationFailsWithTheFailureReason(t *testing.T) {
	for _, test := range []struct{ state, prefix string }{
		{"failed", "Room object publication btr_pub_1 failed: "},
		{"partial", "Room object publication btr_pub_1 was only partially delivered: "},
	} {
		err := testPublication.outcome(objectStatusWithReason(test.state, destinationAccessDenied), false)
		if err == nil || err.Error() != test.prefix+destinationAccessDenied {
			t.Fatalf("state=%s err=%v", test.state, err)
		}
	}
}

// Every terminal state but completed fails a followed publish as a final
// operation failure, of a kind naming the state, with a hint that inspects the
// publication and never suggests starting the agent. --allow-partial accepts
// a partial delivery only.
func TestFollowedPublicationOutcomes(t *testing.T) {
	for _, test := range []struct {
		state        string
		allowPartial bool
		wantKind     string
		wantMessage  string
	}{
		{state: "completed"},
		{state: "completed", allowPartial: true},
		{state: "partial", allowPartial: true},
		{state: "in_progress"},
		{state: "partial", wantKind: "object_publication_partial", wantMessage: "Room object publication btr_pub_1 was only partially delivered."},
		{state: "failed", wantKind: "object_publication_failed", wantMessage: "Room object publication btr_pub_1 failed."},
		{state: "failed", allowPartial: true, wantKind: "object_publication_failed", wantMessage: "Room object publication btr_pub_1 failed."},
		{state: "cancelled", wantKind: "object_publication_cancelled", wantMessage: "Room object publication btr_pub_1 was cancelled."},
		{state: "expired", wantKind: "object_publication_expired", wantMessage: "Room object publication btr_pub_1 expired."},
		{state: "rejected", wantKind: "object_publication_rejected", wantMessage: "Room object publication btr_pub_1 was rejected."},
	} {
		t.Run(fmt.Sprintf("%s allow_partial=%t", test.state, test.allowPartial), func(t *testing.T) {
			err := testPublication.outcome(objectStatusWithReason(test.state, ""), test.allowPartial)
			if test.wantKind == "" {
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				return
			}
			var failure *Failure
			if !errors.As(err, &failure) {
				t.Fatalf("err = %#v, want a *Failure", err)
			}
			if failure.Code != ExitFailed || failure.Kind != test.wantKind || failure.Message != test.wantMessage {
				t.Fatalf("failure = %#v", failure)
			}
			if !strings.Contains(failure.Hint, "`beam room btr_room_a channel btr_channel_files object show btr_pub_1`") {
				t.Fatalf("hint = %q, want the command that inspects the publication", failure.Hint)
			}
			if strings.Contains(strings.ToLower(failure.Hint), "agent") || strings.Contains(strings.ToLower(failure.Message), "agent") {
				t.Fatalf("failure mentions the agent: %#v", failure)
			}
		})
	}
}

func objectStatusWithDeliveries(state, message string, deliveries ...map[string]any) map[string]any {
	response := objectStatusWithReason(state, message)
	list := make([]any, 0, len(deliveries))
	for _, delivery := range deliveries {
		list = append(list, delivery)
	}
	objectStatusView(response)["deliveries"] = list
	return response
}

// A failed publication's message names every recipient that missed the file,
// with the reason its delivery gives, so the cause is visible without
// `object show`.
func TestFollowedPublicationNamesTheRecipientsThatMissedTheFile(t *testing.T) {
	delivered := map[string]any{"member_id": "btr_member_a", "state": "delivered"}
	offline := map[string]any{"member_id": "btr_member_x", "state": "failed", "unavailable_reason": "agent_offline"}
	incapable := map[string]any{"member_id": "btr_member_y", "state": "failed", "unavailable_reason": "incapable"}
	pending := map[string]any{"member_id": "btr_member_z", "state": "pending"}
	for _, test := range []struct {
		name        string
		response    map[string]any
		wantMessage string
	}{
		{"partial without a reason", objectStatusWithDeliveries("partial", "", delivered, offline),
			"Room object publication btr_pub_1 was only partially delivered. It failed for 1 of 2 recipients: btr_member_x (agent_offline)."},
		{"failed with a reason", objectStatusWithDeliveries("failed", destinationAccessDenied, offline, incapable),
			"Room object publication btr_pub_1 failed: " + destinationAccessDenied + " It failed for 2 of 2 recipients: btr_member_x (agent_offline), btr_member_y (incapable)."},
		{"reason without a final stop", objectStatusWithDeliveries("failed", "transfer_expired", offline),
			"Room object publication btr_pub_1 failed: transfer_expired. It failed for 1 of 1 recipient: btr_member_x (agent_offline)."},
		{"delivery state when no reason is given", objectStatusWithDeliveries("cancelled", "", delivered, pending),
			"Room object publication btr_pub_1 was cancelled. It failed for 1 of 2 recipients: btr_member_z (pending)."},
		{"every recipient delivered", objectStatusWithDeliveries("failed", "", delivered),
			"Room object publication btr_pub_1 failed."},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := testPublication.outcome(test.response, false)
			if err == nil || err.Error() != test.wantMessage {
				t.Fatalf("err = %v, want %q", err, test.wantMessage)
			}
		})
	}
}

// A publication to many recipients names the first few that missed the file
// and counts the rest, which `object show` lists.
func TestFollowedPublicationCountsRecipientsBeyondTheNamedOnes(t *testing.T) {
	deliveries := make([]map[string]any, 0, objectMissedRecipientsNamed+3)
	for index := 0; index < objectMissedRecipientsNamed+2; index++ {
		deliveries = append(deliveries, map[string]any{"member_id": fmt.Sprintf("btr_member_%d", index), "state": "failed", "unavailable_reason": "agent_offline"})
	}
	deliveries = append(deliveries, map[string]any{"member_id": "btr_member_done", "state": "delivered"})
	err := testPublication.outcome(objectStatusWithDeliveries("partial", "", deliveries...), false)
	if err == nil {
		t.Fatal("a partial publication must fail")
	}
	message := err.Error()
	if !strings.Contains(message, fmt.Sprintf("It failed for %d of %d recipients: btr_member_0 (agent_offline), ", objectMissedRecipientsNamed+2, objectMissedRecipientsNamed+3)) ||
		!strings.HasSuffix(message, fmt.Sprintf("btr_member_%d (agent_offline), and 2 more.", objectMissedRecipientsNamed-1)) ||
		strings.Contains(message, fmt.Sprintf("btr_member_%d ", objectMissedRecipientsNamed)) {
		t.Fatalf("message = %q", message)
	}
}

func TestObjectStatusWithoutAFailureShowsNoReason(t *testing.T) {
	for _, test := range []struct {
		name, state, message, followErr string
	}{
		{"failed without a reason", "failed", "", "Room object publication btr_pub_1 failed."},
		{"reason while still running", "in_progress", "Transfer admission deadline exceeded.", ""},
		{"reason after completion", "completed", "Provider cleanup could not be verified.", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			runner := Runner{Out: &output}
			response := objectStatusWithReason(test.state, test.message)
			if err := runner.renderObjectSnapshot(outputMode{}, response, "publication"); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "Error") {
				t.Fatalf("output=%q", output.String())
			}
			err := testPublication.outcome(response, false)
			if (err == nil) != (test.followErr == "") || (err != nil && err.Error() != test.followErr) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
