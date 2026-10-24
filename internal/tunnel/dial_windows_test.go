//go:build windows

package tunnel

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestDialLocalUsesWindowsNamedPipe(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\beam-cli-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;OW)",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, _ := listener.Accept()
		accepted <- connection
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := dialLocal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case server := <-accepted:
		if server == nil {
			t.Fatal("named pipe accept failed")
		}
		_ = server.Close()
	case <-ctx.Done():
		t.Fatal("named pipe connection timed out")
	}
}
