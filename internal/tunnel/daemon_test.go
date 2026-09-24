//go:build !windows

package tunnel

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStopIncompatibleBundleUsesRunningDaemonVersion(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(ShutdownResponse{Stopping: true})
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
