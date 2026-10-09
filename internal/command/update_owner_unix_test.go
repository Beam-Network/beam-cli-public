//go:build !windows

package command

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

func TestStopDaemonForUpdateWaitsForSocketOwner(t *testing.T) {
	testDaemonUpdateWaitsForSocketOwner(t, stopDaemonForUpdate)
}

func TestSignalDaemonAfterUpdateWaitsForSocketOwner(t *testing.T) {
	testDaemonUpdateWaitsForSocketOwner(t, signalDaemonAfterUpdate)
}

func testDaemonUpdateWaitsForSocketOwner(t *testing.T, stop func(context.Context, string) error) {
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
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var server *http.Server
	server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/shutdown" {
			http.NotFound(w, request)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"stopping": true})
		go func() {
			time.Sleep(10 * time.Millisecond)
			_ = server.Close()
			time.Sleep(200 * time.Millisecond)
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}()
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := stop(ctx, socket); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("update returned while the old daemon still owned its socket")
	}
}
