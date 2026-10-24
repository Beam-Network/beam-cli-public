//go:build !windows

package tunnel

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
)

// Starting the agent while one is running returns the running agent and starts
// no other.
func TestStartDaemonReturnsTheRunningDaemon(t *testing.T) {
	testutil.IsolateAgent(t)
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != StatusPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Status{Ready: true, AgentVersion: "test", ProtocolVersion: ProtocolVersion})
	}))
	result, err := StartDaemon(context.Background(), StartOptions{SocketPath: socket, CLIVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyRunning || result.PID != 0 || result.Status.ProtocolVersion != ProtocolVersion {
		t.Fatalf("result=%#v", result)
	}
}

func TestStopIncompatibleBundleUsesRunningDaemonVersion(t *testing.T) {
	testStopIncompatibleBundle(t, true)
}

// A daemon may close the connection as it stops, before it replies to the
// shutdown request; the replacement still waits for it to stop.
func TestStopIncompatibleBundleFollowsAnUnansweredShutdown(t *testing.T) {
	testStopIncompatibleBundle(t, false)
}

func testStopIncompatibleBundle(t *testing.T, answer bool) {
	directory, err := os.MkdirTemp("", "beam-daemon-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "agent.sock")
	lock, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	var server *http.Server
	server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != ShutdownPath {
			http.NotFound(w, request)
			return
		}
		if got := request.Header.Get("Beam-CLI-Version"); got != "v0.0.0-dev.old-bundle" {
			t.Errorf("Beam-CLI-Version = %q", got)
		}
		if answer {
			_ = json.NewEncoder(w).Encode(ShutdownResponse{Stopping: true})
		} else if connection, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = connection.Close()
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			_ = server.Close()
			time.Sleep(200 * time.Millisecond)
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}()
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = os.Remove(socket)
	})

	started := time.Now()
	replaced, err := stopIncompatibleBundle(context.Background(), socket, &Error{
		Kind: ErrVersion,
		Code: "bundle_version_incompatible",
		Details: map[string]any{
			"daemon_version": "v0.0.0-dev.old-bundle",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("bundle mismatch was not handled")
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("replacement began before the old daemon released socket ownership")
	}
}

func TestStopIncompatibleBundleIgnoresOtherVersionErrors(t *testing.T) {
	replaced, err := stopIncompatibleBundle(context.Background(), filepath.Join(t.TempDir(), "agent.sock"), &Error{
		Kind: ErrVersion,
		Code: "protocol_incompatible",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replaced {
		t.Fatal("protocol mismatch must not stop the running daemon")
	}
}

// The daemon outlives `beam agent connect`, so a BEAM_API_KEY exported for the
// CLI must not reach the process it starts.
func TestStartDaemonDoesNotPassAPIKeyToDaemon(t *testing.T) {
	directory := t.TempDir()
	envFile := filepath.Join(directory, "daemon.env")
	binary := filepath.Join(directory, "beam-tunnel-agent")
	script := "#!/bin/sh\nenv > \"$BEAM_TEST_DAEMON_ENV_FILE\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEAM_TEST_DAEMON_ENV_FILE", envFile)
	t.Setenv("BEAM_API_KEY", "b1m_daemon_must_not_see_this")
	socketDirectory, err := os.MkdirTemp("", "beam-env-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = StartDaemon(ctx, StartOptions{
		SocketPath:  filepath.Join(socketDirectory, "agent.sock"),
		AgentBinary: binary,
		LogPath:     filepath.Join(directory, "daemon.log"),
	})
	if err == nil {
		t.Fatal("the fake daemon exits immediately, so StartDaemon must fail")
	}
	environment, readErr := os.ReadFile(envFile)
	if readErr != nil {
		t.Fatalf("fake daemon did not run (start error: %v): %v", err, readErr)
	}
	if strings.Contains(string(environment), "BEAM_API_KEY") {
		t.Fatalf("daemon environment contains BEAM_API_KEY:\n%s", environment)
	}
	if !strings.Contains(string(environment), "BEAM_TEST_DAEMON_ENV_FILE=") {
		t.Fatalf("daemon environment lost unrelated variables:\n%s", environment)
	}
}
