//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestTunnelEndpointsAgainstFakeDaemon(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, AgentVersion: "test", ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.EndpointsPath:
			_ = json.NewEncoder(w).Encode(tunnel.EndpointsResponse{
				Endpoints: []tunnel.Endpoint{{ID: "ep_test", Direction: "source", Kind: "file", Status: "active"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"tunnel", "endpoints", "--json"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var got tunnel.EndpointsResponse
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Endpoints) != 1 || got.Endpoints[0].ID != "ep_test" {
		t.Fatalf("result=%#v", got)
	}
}

func TestTunnelRoomDispatchesIntegratedClient(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", "/tmp/beam-integrated-room-test.sock")
	var gotArgs []string
	var gotSocket string
	app := New(version.BuildInfo{Version: "test"})
	app.roomRun = func(_ context.Context, args []string, socket, version string, renderer output.Renderer) error {
		gotArgs = append([]string(nil), args...)
		gotSocket = socket
		if version != "test" {
			t.Fatalf("version = %q", version)
		}
		if renderer.Mode != output.JSON {
			t.Fatalf("renderer mode = %q, want json", renderer.Mode)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"tunnel", "room", "create", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if gotSocket != "/tmp/beam-integrated-room-test.sock" {
		t.Fatalf("socket = %q", gotSocket)
	}
	if len(gotArgs) != 1 || gotArgs[0] != "create" {
		t.Fatalf("args = %#v", gotArgs)
	}
}

func TestTunnelRoomCreateCallsDaemonWithoutCompanion(t *testing.T) {
	var called bool
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Path != "/v1/rooms" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Beam-Local-API-Min"); got != fmt.Sprint(roomcli.MinimumLocalAPIVersion) {
			t.Fatalf("local API min = %q, want %d", got, roomcli.MinimumLocalAPIVersion)
		}
		if got := r.Header.Get("Beam-Local-API-Max"); got != fmt.Sprint(tunnel.ProtocolVersion) {
			t.Fatalf("local API max = %q", got)
		}
		if got := r.Header.Get("Beam-CLI-Version"); got != "test" {
			t.Fatalf("CLI version = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "room-create-test" {
			t.Fatalf("idempotency key = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		// Starting a room is billable, so the create names the Beam API key that
		// pays for it.
		if body["api_key"] != "bm_live_payer" {
			t.Fatalf("body = %#v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"room":{"room_id":"btr_room_test","state":"active"},"membership":{"member_id":"btr_member_test","state":"active"},"authorization_epoch":1,"resume":{"notification_cursor":1}}`))
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"tunnel", "room", "create", "--idempotency-key", "room-create-test", "--api-key", "bm_live_payer", "--json"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !called {
		t.Fatal("daemon was not called")
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	room, _ := got["room"].(map[string]any)
	if room["room_id"] != "btr_room_test" {
		t.Fatalf("result=%#v", got)
	}
}

// A room with no payer is refused before the daemon is called: the coordinator
// would refuse it anyway, and a local error names the missing flag.
func TestRoomCreateWithoutAPIKeyNeverReachesTheDaemon(t *testing.T) {
	var called bool
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	t.Setenv("BEAM_API_KEY", "")
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(), []string{"room", "create"}, &stdout, &stderr)
	if code != roomcli.ExitUsage || called {
		t.Fatalf("code=%d called=%t stderr=%s", code, called, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--api-key") {
		t.Fatalf("stderr=%s", stderr.String())
	}
}

// The key may come from the environment instead, so automation does not repeat
// a secret on every command line.
func TestRoomCreateReadsTheKeyFromTheEnvironment(t *testing.T) {
	var payer any
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		payer = body["api_key"]
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"room":{"room_id":"btr_room_test","state":"active"},"membership":{"member_id":"btr_member_test","state":"active"},"authorization_epoch":1,"resume":{"notification_cursor":1}}`))
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	t.Setenv("BEAM_API_KEY", "bm_live_env_payer")
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(), []string{"room", "create", "--json"}, &stdout, &stderr)
	if code != 0 || payer != "bm_live_env_payer" {
		t.Fatalf("code=%d payer=%#v stderr=%s", code, payer, stderr.String())
	}
}

func TestRoomNamespaceCallsSameIntegratedDaemonClient(t *testing.T) {
	var called bool
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodGet || r.URL.Path != "/v1/rooms" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Beam-Local-API-Min"); got != fmt.Sprint(roomcli.MinimumLocalAPIVersion) {
			t.Fatalf("local API min = %q, want %d", got, roomcli.MinimumLocalAPIVersion)
		}
		_, _ = w.Write([]byte(`{"rooms":[]}`))
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"room", "list", "--json"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !called || stdout.String() != "{\"rooms\":[]}\n" {
		t.Fatalf("called=%t stdout=%q", called, stdout.String())
	}
}

func TestTunnelStudioConnectAndStatusAgainstFakeDaemon(t *testing.T) {
	var received tunnel.StudioConnectionRequest
	status := tunnel.StudioConnectionStatus{
		Configured: true, Connected: true, State: "connected",
		StudioURL: "https://studio.example.com", AgentID: "agent_test",
	}
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tunnel.StatusPath && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
			return
		}
		if r.URL.Path != tunnel.StudioConnectionsPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Beam-Local-API-Max") != fmt.Sprint(tunnel.ProtocolVersion) {
			t.Fatalf("local API max = %q, want %d", r.Header.Get("Beam-Local-API-Max"), tunnel.ProtocolVersion)
		}
		switch r.Method {
		case http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
		case http.MethodGet:
		default:
			t.Fatalf("method = %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	app := New(version.BuildInfo{Version: "test"})
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{
		"tunnel", "studio", "connect", "https://studio.example.com",
		"--code", "BM-TEST", "--name", "edge-test",
		"--allow-root", "/srv/beam", "--allow-kind=file",
		"--allow-target", "127.0.0.1", "--allow-public",
		"--max-tunnels", "8", "--max-operations=2",
		"--max-command-ttl", "900", "--rooms=false", "--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("connect code=%d stderr=%s", code, stderr.String())
	}
	if received.StudioURL != status.StudioURL || received.EnrollmentCode != "BM-TEST" || received.MachineName != "edge-test" {
		t.Fatalf("request identity = %#v", received)
	}
	if len(received.FilesystemRoots) != 1 || len(received.TunnelKinds) != 1 || len(received.NetworkTargets) != 1 {
		t.Fatalf("request allowlists = %#v", received)
	}
	if !received.AllowPublic || received.RoomsEnabled || received.MaxTunnels != 8 || received.MaxConcurrentOperations != 2 || received.MaxCommandTTLSeconds != 900 {
		t.Fatalf("request policy = %#v", received)
	}

	stdout.Reset()
	stderr.Reset()
	code = app.Run(context.Background(), []string{"tunnel", "studio", "status", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status code=%d stderr=%s", code, stderr.String())
	}
	var got tunnel.StudioConnectionStatus
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Connected || got.AgentID != "agent_test" {
		t.Fatalf("status = %#v", got)
	}
}

func TestTunnelReceiveCreatesDestination(t *testing.T) {
	directory := t.TempDir()
	parent := filepath.Dir(directory)
	t.Chdir(parent)
	relativeDirectory := "./" + filepath.Base(directory)
	var received tunnel.DestinationRequest
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, AgentVersion: "test", ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.DestinationsPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Endpoint{ID: "ep_destination", Direction: "destination", Kind: "directory", Target: received.Directory, Status: "active"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"tunnel", "receive", relativeDirectory, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var got tunnel.Endpoint
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "ep_destination" || got.Direction != "destination" {
		t.Fatalf("result=%#v", got)
	}
	if received.Directory != directory {
		t.Fatalf("directory = %q, want %q", received.Directory, directory)
	}
	if !received.Public || received.Standbys != 1 || received.ShutdownGrace != "5s" {
		t.Fatalf("public defaults = %#v", received)
	}
}

func TestTunnelExposeSendsAbsoluteFilePath(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "source.bin")
	if err := os.WriteFile(file, []byte("beam"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(directory)
	var received tunnel.ExposureRequest
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, AgentVersion: "test", ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.ExposuresPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Endpoint{ID: "ep_source", Direction: "source", Kind: "file", Target: received.Target, Status: "active"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"tunnel", "expose", "file", "./source.bin", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if received.Target != file {
		t.Fatalf("target = %q, want %q", received.Target, file)
	}
	if !received.Public || received.Standbys != 1 || received.ShutdownGrace != "5s" {
		t.Fatalf("public defaults = %#v", received)
	}
}

func TestTunnelExposeObjectStoragePrintsProviderConfig(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "source.bin")
	if err := os.WriteFile(file, []byte("beam"), 0o600); err != nil {
		t.Fatal(err)
	}
	var received tunnel.ExposureRequest
	config := &tunnel.ObjectStorageConfig{
		Provider: "beam-tunnel", Driver: "s3-compatible", EndpointURL: "https://sixabc.tunnel.b3m.dev",
		ForcePathStyle: true, Region: "global", Bucket: "beam", Key: "source.bin",
		AccessKeyID: "ep_source", SecretAccessKey: "public-secret",
	}
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.ExposuresPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Endpoint{ID: "ep_source", Status: "active", ObjectStorage: config})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{
		"tunnel", "expose", "file", file, "--object-storage",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !received.ObjectStorage {
		t.Fatalf("request = %#v", received)
	}
	var got tunnel.ObjectStorageConfig
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not config JSON: %v; stdout=%s", err, stdout.String())
	}
	if got.EndpointURL != config.EndpointURL || got.SecretAccessKey != config.SecretAccessKey {
		t.Fatalf("config = %#v", got)
	}
}

func TestTunnelReceiveObjectStorageWritesProviderConfig(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "object-storage.json")
	var received tunnel.DestinationRequest
	config := &tunnel.ObjectStorageConfig{
		Provider: "beam-tunnel", Driver: "s3-compatible", EndpointURL: "https://dest01.tunnel.b3m.dev",
		ForcePathStyle: true, Region: "global", Bucket: "beam", Key: "objects",
		AccessKeyID: "ep_destination", SecretAccessKey: "public-secret",
	}
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.DestinationsPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Endpoint{ID: "ep_destination", Status: "active", ObjectStorage: config})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{
		"tunnel", "receive", directory, "--object-storage", "--object-storage-config", configPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !received.ObjectStorage {
		t.Fatalf("request = %#v", received)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	var got tunnel.ObjectStorageConfig
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != "objects" || got.SecretAccessKey != config.SecretAccessKey {
		t.Fatalf("config = %#v", got)
	}
}

func TestTunnelExposePropagatesPublicOptions(t *testing.T) {
	var received tunnel.ExposureRequest
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.ExposuresPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Endpoint{ID: "ep_stream", Kind: received.Kind, Status: "active", PublicURL: "https://public.test"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{
		"tunnel", "expose", "stream", "8080",
		"--public-endpoint-key", "my-app", "--standbys", "2",
		"--relay-id", "relay-edge-ams-01", "--shutdown-grace", "7s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if received.Kind != "stream" || !received.Public || received.PublicEndpointKey != "my-app" ||
		received.Standbys != 2 || received.RelayID != "relay-edge-ams-01" || received.ShutdownGrace != "7s" {
		t.Fatalf("request = %#v", received)
	}
	if stdout.String() != "https://public.test\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestTunnelExposeNoPublicUsesLegacyMode(t *testing.T) {
	var received tunnel.ExposureRequest
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.ExposuresPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Endpoint{ID: "ep_http", Kind: received.Kind, Status: "active"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{
		"tunnel", "expose", "http", "8080", "--no-public", "--relay-id", "relay-test",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if received.Public || received.Standbys != 0 || received.ShutdownGrace != "" || received.RelayID != "relay-test" {
		t.Fatalf("request = %#v", received)
	}
}

func TestTunnelExposeRejectsNativePublicTCP(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", directory)
	t.Setenv("BEAM_AGENT_SOCKET", filepath.Join(directory, "missing-agent.sock"))
	app := New(version.BuildInfo{Version: "test"})
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		t.Fatal("invalid arguments must not start beam-agentd")
		return tunnel.StartResult{}, nil
	}
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{
		"tunnel", "expose", "tcp", "127.0.0.1:9000", "--public",
	}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tcp does not support --public") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestTunnelStatusPreservesAutoStartFailure(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", directory)
	t.Setenv("BEAM_AGENT_SOCKET", filepath.Join(directory, "missing-agent.sock"))
	app := New(version.BuildInfo{Version: "test"})
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		return tunnel.StartResult{}, fmt.Errorf("start failed")
	}

	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"tunnel", "status"}, &stdout, &stderr)
	if code != ExitDaemonUnavailable {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Could not start "+version.AgentName()) {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestTunnelExposeAcceptsWebRTC(t *testing.T) {
	var received tunnel.ExposureRequest
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.ExposuresPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Endpoint{ID: "ep_webrtc", Kind: received.Kind, Status: "active"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{
		"tunnel", "expose", "webrtc", "8080",
	}, &stdout, &stderr)
	if code != 0 || received.Kind != "webrtc" || !received.Public {
		t.Fatalf("code=%d request=%#v stderr=%s", code, received, stderr.String())
	}
}

func TestTunnelStartIsIdempotentWhenDaemonIsRunning(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tunnel.StatusPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, AgentVersion: "test", ProtocolVersion: tunnel.ProtocolVersion})
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"tunnel", "start", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var got tunnel.StartResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.AlreadyRunning || got.Status.ProtocolVersion != tunnel.ProtocolVersion {
		t.Fatalf("result=%#v", got)
	}
}

func TestTunnelStopUsesDaemonAPI(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, AgentVersion: "test", ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.ShutdownPath:
			_ = json.NewEncoder(w).Encode(tunnel.ShutdownResponse{Stopping: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"tunnel", "stop", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var got tunnel.ShutdownResponse
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Stopping {
		t.Fatalf("result=%#v", got)
	}
}

func TestIncompatibleDaemonHasStableExitCode(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion + 1})
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"tunnel", "status"},
		&stdout,
		&stderr,
	)
	if code != ExitVersionIncompatible {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestTunnelUploadCreatesDaemonOperation(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "input.bin")
	session := filepath.Join(directory, "session.json")
	if err := os.WriteFile(file, []byte("beam"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var received tunnel.OperationRequest
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, DaemonVersion: "test", ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case tunnel.OperationsPath:
			_ = json.NewDecoder(r.Body).Decode(&received)
			_ = json.NewEncoder(w).Encode(tunnel.Operation{ID: "op_upload", Type: received.Type, Status: "queued", Request: received})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{
		"tunnel", "upload", file, "--session", session, "--concurrency", "8", "--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if received.Type != "upload" || received.File != file || received.Session != session || received.Concurrency != 8 {
		t.Fatalf("request=%#v", received)
	}
}

func TestTunnelOperationInspectionAndCancellation(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tunnel.StatusPath {
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, DaemonVersion: "test", ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
			return
		}
		if r.URL.Path == tunnel.OperationsPath+"/op_test" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(tunnel.Operation{ID: "op_test", Type: "download", Status: "running"})
			return
		}
		if r.URL.Path == tunnel.OperationsPath+"/op_test/cancel" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(tunnel.Operation{ID: "op_test", Type: "download", Status: "cancelled"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	for _, args := range [][]string{{"tunnel", "operation", "op_test"}, {"tunnel", "cancel", "op_test"}} {
		var stdout, stderr bytes.Buffer
		if code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
	}
}
