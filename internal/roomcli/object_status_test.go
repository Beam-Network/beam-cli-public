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
