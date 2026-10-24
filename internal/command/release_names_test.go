//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// Room notices reach the terminal after a successful command, outside the
// error renderer, and still name the running binary; so do usage lines, which
// end up in the error message.
func TestRoomCommandsNameTheRunningBinary(t *testing.T) {
	original := version.Version
	version.Version = "v0.0.0-dev.c85930cb2c64.7267ac677c0d"
	t.Cleanup(func() { version.Version = original })
	// A development version enables the update notice, which reads the CDN.
	t.Setenv("BEAM_NO_UPDATE_NOTICE", "1")
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/rooms":
			_, _ = w.Write([]byte(`{"rooms":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"room", "list"}, want: []string{"beam-dev room: no rooms.", `"beam-dev room create --api-key $BEAM_API_KEY"`}},
		{args: []string{"tunnel", "room", "channel", "bogus"}, want: []string{"usage: beam-dev room [--json|--quiet]", "\n       beam-dev room channel object"}},
	} {
		var stdout, stderr bytes.Buffer
		New(version.BuildInfo{Version: version.Version}).Run(context.Background(), test.args, &stdout, &stderr)
		for _, want := range test.want {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("%v: stderr lacks %q:\n%s", test.args, want, stderr.String())
			}
		}
		if strings.Contains(stdout.String()+stderr.String(), "beam room") {
			t.Fatalf("%v names the production binary:\nstdout=%s\nstderr=%s", test.args, stdout.String(), stderr.String())
		}
	}
}
