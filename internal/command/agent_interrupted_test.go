//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestMapTunnelErrorSeparatesUnreachableFromInterrupted(t *testing.T) {
	for _, test := range []struct {
		kind     tunnel.ErrorKind
		wantCode int
		wantKind string
	}{
		{kind: tunnel.ErrUnavailable, wantCode: ExitDaemonUnavailable},
		{kind: tunnel.ErrInterrupted, wantCode: ExitOperationFailed, wantKind: errorKindAgentRequestInterrupted},
	} {
		var cliErr *Error
		if !errors.As(mapTunnelError(&tunnel.Error{Kind: test.kind}), &cliErr) || cliErr.Code != test.wantCode || cliErr.Kind != test.wantKind {
			t.Fatalf("%s: error = %#v", test.kind, cliErr)
		}
	}
}

// An agent that accepts the connection and closes it without answering is
// running, so `agent status` reports an interrupted request, not a stopped
// agent.
func TestAgentStatusReportsAnInterruptedRequest(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if connection, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = connection.Close()
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"agent", "status", "--json"}, &stdout, &stderr)
	if code != ExitOperationFailed {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var payload struct {
		Error struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil || payload.Error.Kind != errorKindAgentRequestInterrupted {
		t.Fatalf("stderr=%q err=%v", stderr.String(), err)
	}
}

// Starting the agent reports a running agent that did not answer as such,
// not as a failure to start it.
func TestAgentStartReportsAnInterruptedRequest(t *testing.T) {
	testutil.IsolateAgent(t)
	app := New(version.BuildInfo{Version: "test"})
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		return tunnel.StartResult{}, &tunnel.Error{Kind: tunnel.ErrInterrupted}
	}
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"agent", "start"}, &stdout, &stderr); code != ExitOperationFailed {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}
