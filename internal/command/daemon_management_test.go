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

func TestOperationPruneAndStudioPermissionsUseDaemonRoutes(t *testing.T) {
	pruned := false
	var received tunnel.StudioPermissionsPatch
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case request.Method == http.MethodDelete && request.URL.Path == tunnel.OperationsPath:
			pruned = true
			_ = json.NewEncoder(w).Encode(tunnel.OperationPruneResponse{RemovedIDs: []string{"op-finished"}})
		case request.Method == http.MethodPatch && request.URL.Path == tunnel.StudioConnectionsPath+"/permissions":
			if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(tunnel.StudioConnectionStatus{Configured: true, State: "reconnecting", Permissions: &tunnel.StudioPermissions{MaxTunnels: 7}})
		default:
			http.NotFound(w, request)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	app := New(version.BuildInfo{Version: "test"})

	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"operation", "prune"}, &stdout, &stderr); code != ExitOK || !pruned {
		t.Fatalf("prune code=%d called=%t stderr=%s", code, pruned, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run(context.Background(), []string{"studio", "update-permissions", "--allow-root", "/srv/beam", "--allow-public=false", "--max-tunnels", "7", "--rooms=true"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("studio code=%d stderr=%s", code, stderr.String())
	}
	if received.FilesystemRoots == nil || len(*received.FilesystemRoots) != 1 || (*received.FilesystemRoots)[0] != "/srv/beam" || received.AllowPublic == nil || *received.AllowPublic || received.MaxTunnels == nil || *received.MaxTunnels != 7 || received.RoomsEnabled == nil || !*received.RoomsEnabled {
		t.Fatalf("studio patch=%#v", received)
	}
}

// The log feed is filtered client-side, so an unknown operation matches nothing
// and would look like a clean run unless the ID is resolved up front.
func TestOperationLogsRejectsAnUnknownOperation(t *testing.T) {
	const known = "op-known"
	logsServed := false
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case request.Method == http.MethodGet && request.URL.Path == tunnel.OperationsPath+"/"+known:
			_ = json.NewEncoder(w).Encode(tunnel.Operation{ID: known, Type: "upload", Status: "running"})
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, tunnel.OperationsPath+"/"):
			http.NotFound(w, request)
		case request.Method == http.MethodGet && request.URL.Path == tunnel.LogsPath:
			logsServed = true
			_ = json.NewEncoder(w).Encode(tunnel.LogEvent{Message: "started", Fields: map[string]any{"operation_id": known}})
		default:
			http.NotFound(w, request)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	app := New(version.BuildInfo{Version: "test"})

	for _, command := range [][]string{
		{"operation", "logs", "op-missing"},
		{"transfer", "logs", "op-missing"},
	} {
		var stdout, stderr bytes.Buffer
		code := app.Run(context.Background(), command, &stdout, &stderr)
		if code != ExitNotFound {
			t.Fatalf("%v code=%d want=%d stdout=%q stderr=%q", command, code, ExitNotFound, stdout.String(), stderr.String())
		}
		if logsServed {
			t.Fatalf("%v streamed logs for an operation that does not exist", command)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"operation", "logs", known}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !logsServed || !strings.Contains(stdout.String(), "started") {
		t.Fatalf("served=%t stdout=%q", logsServed, stdout.String())
	}
}
