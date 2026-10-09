package roomcli

import (
	"bytes"
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

func TestFollowedPublicationFailsWithTheFailureReason(t *testing.T) {
	for _, test := range []struct{ state, prefix string }{
		{"failed", "room object publication failed: "},
		{"partial", "room object publication only partially delivered under strict completion: "},
	} {
		err := objectFollowResult(objectStatusWithReason(test.state, destinationAccessDenied), false)
		if err == nil || err.Error() != test.prefix+destinationAccessDenied {
			t.Fatalf("state=%s err=%v", test.state, err)
		}
	}
}

func TestObjectStatusWithoutAFailureShowsNoReason(t *testing.T) {
	for _, test := range []struct {
		name, state, message, followErr string
	}{
		{"failed without a reason", "failed", "", "room object publication failed"},
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
			err := objectFollowResult(response, false)
			if (err == nil) != (test.followErr == "") || (err != nil && err.Error() != test.followErr) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
