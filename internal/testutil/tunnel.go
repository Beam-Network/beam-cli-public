//go:build !windows

package testutil

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func LocalHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "beam-test-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = os.RemoveAll(dir)
	})
	return socket
}
