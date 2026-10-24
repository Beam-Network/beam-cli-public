//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// Stopping an agent that is not running has nothing to do, so it succeeds and
// says so instead of reporting the agent unavailable.
func TestAgentStopOnAStoppedAgentSucceeds(t *testing.T) {
	testutil.IsolateAgent(t)
	app := New(version.BuildInfo{Version: "test"})
	app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
		return tunnel.StartResult{}, errors.New("stop must not start the daemon")
	}
	for args, want := range map[string]string{
		"agent stop":        version.AgentName() + " is not running\n",
		"agent stop --json": `{"stopped":true,"was_running":false}`,
	} {
		var stdout, stderr bytes.Buffer
		if code := app.Run(context.Background(), strings.Fields(args), &stdout, &stderr); code != ExitOK {
			t.Fatalf("%s: code=%d stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("%s: stdout=%q, want %q", args, stdout.String(), want)
		}
	}
}

// `agent stop` returns only once the daemon has exited and released its
// socket, so an `agent start` that follows immediately can bind it. The daemon
// may also close the connection as it stops, before it replies.
func TestAgentStopWaitsForTheDaemonToExit(t *testing.T) {
	for name, answer := range map[string]bool{"answered": true, "unanswered": false} {
		t.Run(name, func(t *testing.T) {
			socket, shutdowns := stoppableDaemon(t, answer)
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			started := time.Now()
			var stdout, stderr bytes.Buffer
			code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"agent", "stop", "--json"}, &stdout, &stderr)
			if code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
				t.Fatalf("agent stop returned after %s, while the daemon still owned its socket", elapsed)
			}
			if shutdowns.Load() != 1 {
				t.Fatalf("shutdown requests=%d, want 1", shutdowns.Load())
			}
			var result agentStopResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || !result.Stopped || !result.WasRunning {
				t.Fatalf("stdout=%q err=%v", stdout.String(), err)
			}
		})
	}
}

// stoppableDaemon serves a compatible daemon on a socket whose ownership lock
// it holds. A shutdown request closes the server and releases the lock 200 ms
// later, like a daemon finishing its shutdown.
func stoppableDaemon(t *testing.T, answer bool) (string, *atomic.Int32) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "beam-")
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
	var shutdowns atomic.Int32
	var server *http.Server
	server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
		case request.Method == http.MethodPost && request.URL.Path == tunnel.ShutdownPath:
			shutdowns.Add(1)
			if answer {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(tunnel.ShutdownResponse{Stopping: true})
			} else if connection, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = connection.Close()
			}
			go func() {
				time.Sleep(10 * time.Millisecond)
				_ = server.Close()
				time.Sleep(200 * time.Millisecond)
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			}()
		default:
			http.NotFound(w, request)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket, &shutdowns
}
