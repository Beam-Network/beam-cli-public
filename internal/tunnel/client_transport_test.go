//go:build !windows

package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// socketPath returns a socket path short enough for every platform's limit.
func socketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "beam-dial-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "agent.sock")
}

// rawAgent listens on a socket and hands every accepted connection to serve.
func rawAgent(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	path := socketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return path
}

// readRequest reads one request's headers, so the agent has received it.
func readRequest(conn net.Conn) {
	_, _ = http.ReadRequest(bufio.NewReader(conn))
}

func transportKind(t *testing.T, err error) ErrorKind {
	t.Helper()
	var tunnelErr *Error
	if !errors.As(err, &tunnelErr) {
		t.Fatalf("error = %#v, want a *tunnel.Error", err)
	}
	return tunnelErr.Kind
}

// A request that cannot connect to the agent's socket never reached it and
// reports the agent unavailable; one that connected and then got no answer
// may have reached it and reports the request interrupted.
func TestLocalClientClassifiesTransportFailures(t *testing.T) {
	refused := func(t *testing.T) string {
		path := socketPath(t)
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = listener.Close()
		return path
	}
	for _, test := range []struct {
		name   string
		socket func(*testing.T) string
		want   ErrorKind
	}{
		{name: "no socket", socket: socketPath, want: ErrUnavailable},
		{name: "connection refused", socket: refused, want: ErrUnavailable},
		{name: "closed on accept", socket: func(t *testing.T) string {
			return rawAgent(t, func(conn net.Conn) { _ = conn.Close() })
		}, want: ErrInterrupted},
		{name: "closed after reading the request", socket: func(t *testing.T) string {
			return rawAgent(t, func(conn net.Conn) {
				readRequest(conn)
				_ = conn.Close()
			})
		}, want: ErrInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewLocalClient(test.socket(t))
			err := client.Do(context.Background(), http.MethodPost, "/v1/rooms/r/channels/c/objects/publish", "", map[string]any{"file": "f"}, nil)
			if got := transportKind(t, err); got != test.want {
				t.Fatalf("kind = %q, want %q (err = %v)", got, test.want, err)
			}
		})
	}
}

func TestLocalClientResponseTimeoutIsInterrupted(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	client := NewLocalClient(rawAgent(t, func(conn net.Conn) {
		readRequest(conn)
		<-release
		_ = conn.Close()
	}))
	client.HTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout = 100 * time.Millisecond
	_, err := client.Status(context.Background())
	if got := transportKind(t, err); got != ErrInterrupted {
		t.Fatalf("kind = %q, want %q (err = %v)", got, ErrInterrupted, err)
	}
}

// A request whose context ends before it connects never reached the agent.
func TestLocalClientContextEndedBeforeConnectingIsUnavailable(t *testing.T) {
	client := NewLocalClient(rawAgent(t, func(conn net.Conn) {
		t.Error("the agent received a connection")
		_ = conn.Close()
	}))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := client.Status(ctx)
	if got := transportKind(t, err); got != ErrUnavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("kind = %q, want %q (err = %v)", got, ErrUnavailable, err)
	}
}

// The final error decides: a dial failure after a connection was obtained
// comes from the HTTP client resending on a new connection a request that a
// reused one failed before writing any of it.
func TestTransportErrorRules(t *testing.T) {
	dial := &url.Error{Op: "Post", URL: "http://beam-agent/v1/x", Err: &dialError{err: syscall.ENOENT}}
	broken := &url.Error{Op: "Post", URL: "http://beam-agent/v1/x", Err: io.EOF}
	for _, test := range []struct {
		name      string
		err       error
		connected bool
		want      ErrorKind
	}{
		{"dial failed", dial, false, ErrUnavailable},
		{"dial failed on the resend", dial, true, ErrUnavailable},
		{"never connected", broken, false, ErrUnavailable},
		{"connection broke", broken, true, ErrInterrupted},
	} {
		if got := transportError(test.err, test.connected); got.Kind != test.want || !errors.Is(got, test.err) {
			t.Fatalf("%s: error = %#v, want kind %q wrapping the cause", test.name, got, test.want)
		}
	}
}

// An agent that stopped between two requests of one command, closing the
// connection the first one used, is reported unavailable.
func TestLocalClientAgentStoppedBetweenRequestsIsUnavailable(t *testing.T) {
	path := socketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ready":true}`))
	})}
	go func() { _ = server.Serve(listener) }()
	client := NewLocalClient(path)
	if _, err := client.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	_, err = client.Status(context.Background())
	if got := transportKind(t, err); got != ErrUnavailable {
		t.Fatalf("kind = %q, want %q (err = %v)", got, ErrUnavailable, err)
	}
}
