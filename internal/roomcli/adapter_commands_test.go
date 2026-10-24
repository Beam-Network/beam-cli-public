//go:build !windows

package roomcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

// adapterRequest is one request the fake daemon received.
type adapterRequest struct {
	method string
	path   string
	query  url.Values
	body   string
}

// adapterRunner answers stream and datagram requests successfully and records
// them.
func adapterRunner(t *testing.T, stdin string) (Runner, func() []adapterRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []adapterRequest
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, adapterRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query(), body: string(body)})
		mu.Unlock()
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Trailer", streamEndTrailer)
			_, _ = w.Write([]byte("received"))
			w.Header().Set(streamEndTrailer, "eof")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"flow_id": "btr_flow_1"})
	}))
	runner := Runner{Client: tunnel.NewLocalClient(socket), In: strings.NewReader(stdin), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	return runner, func() []adapterRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]adapterRequest(nil), requests...)
	}
}

// Beam picks the Worker that serves a stream, so stream commands refuse the
// relay and transport flags and send no placement or transport selector.
func TestStreamCommandsTakeNoRelayOrTransportFlags(t *testing.T) {
	for _, action := range []string{"publish", "listen"} {
		for _, flag := range [][]string{
			{"--relay", "relay-a"}, {"--standby-relay", "relay-b"}, {"--replace-primary"}, {"--transport", "auto"},
		} {
			t.Run(action+" "+flag[0], func(t *testing.T) {
				runner, requests := adapterRunner(t, "payload")
				args := append([]string{"tunnel", "room", "channel", "stream", "btr_room_a", action, "btr_channel_a"}, flag...)
				failure := runner.Execute(context.Background(), args)
				if failure == nil || failure.Code != ExitUsage {
					t.Fatalf("failure=%+v, want a usage error", failure)
				}
				if sent := requests(); len(sent) != 0 {
					t.Fatalf("requests=%+v, want none", sent)
				}
			})
		}
	}
}

func TestStreamCommandsSendNoPlacementOrTransport(t *testing.T) {
	for _, test := range []struct {
		action, method, body string
	}{
		{action: "publish", method: http.MethodPost, body: "payload"},
		{action: "listen", method: http.MethodGet},
	} {
		t.Run(test.action, func(t *testing.T) {
			runner, requests := adapterRunner(t, "payload")
			args := []string{"tunnel", "room", "channel", "stream", "btr_room_a", test.action, "btr_channel_a"}
			if failure := runner.Execute(context.Background(), args); failure != nil {
				t.Fatalf("failure=%+v", failure)
			}
			sent := requests()
			if len(sent) != 1 {
				t.Fatalf("requests=%+v, want one", sent)
			}
			request := sent[0]
			if request.method != test.method || request.path != "/v1/rooms/btr_room_a/channels/btr_channel_a/stream" ||
				len(request.query) != 0 || request.body != test.body {
				t.Fatalf("request=%+v", request)
			}
		})
	}
}

func TestDatagramCommandsKeepRelayAndTransportFlags(t *testing.T) {
	runner, requests := adapterRunner(t, "")
	send := []string{"tunnel", "room", "channel", "datagram", "btr_room_a", "send", "btr_channel_a",
		"--literal", "event", "--ttl-millis", "5000", "--transport", "quic", "--relay", "relay-a", "--standby-relay", "relay-b", "--replace-primary"}
	if failure := runner.Execute(context.Background(), send); failure != nil {
		t.Fatalf("send failure=%+v", failure)
	}
	listen := []string{"tunnel", "room", "channel", "datagram", "btr_room_a", "listen", "btr_channel_a", "--transport", "v1"}
	if failure := runner.Execute(context.Background(), listen); failure != nil {
		t.Fatalf("listen failure=%+v", failure)
	}
	sent := requests()
	if len(sent) != 2 {
		t.Fatalf("requests=%+v, want two", sent)
	}
	want := url.Values{"transport": {"quic"}, "relay_id": {"relay-a"}, "standby_relay_id": {"relay-b"},
		"replace_primary": {"true"}, "ttl_millis": {"5000"}}
	if sent[0].method != http.MethodPost || sent[0].body != "event" || sent[0].query.Encode() != want.Encode() {
		t.Fatalf("send request=%+v", sent[0])
	}
	if sent[1].method != http.MethodGet || sent[1].query.Get("transport") != "v1" {
		t.Fatalf("listen request=%+v", sent[1])
	}
	invalid := []string{"tunnel", "room", "channel", "datagram", "btr_room_a", "listen", "btr_channel_a", "--transport", "tcp"}
	if failure := runner.Execute(context.Background(), invalid); failure == nil || failure.Code != ExitUsage {
		t.Fatalf("failure=%+v, want a usage error for an unknown transport", failure)
	}
}

// streamListenRunner answers a stream listen with "partial" and then ends the
// response with end.
func streamListenRunner(t *testing.T, end func(w http.ResponseWriter, r *http.Request)) (Runner, *bytes.Buffer) {
	t.Helper()
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Trailer", strings.Join([]string{streamEndTrailer, streamErrorCodeTrailer, streamErrorMessageTrailer, streamErrorReasonsTrailer}, ", "))
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		end(w, r)
	}))
	var stdout bytes.Buffer
	return Runner{Client: tunnel.NewLocalClient(socket), Out: &stdout, Err: &bytes.Buffer{}}, &stdout
}

var streamListenArgs = []string{"tunnel", "room", "channel", "stream", "btr_room_a", "listen", "btr_channel_a"}

// Output has started when a listened stream ends, so the agent reports how it
// ended in trailers; a listen that was cut short fails like the agent's error.
func TestStreamListenReadsHowTheStreamEnded(t *testing.T) {
	for _, test := range []struct {
		name        string
		end         func(w http.ResponseWriter, r *http.Request)
		wantCode    int
		wantKind    string
		wantMessage string
	}{
		{
			name: "complete stream",
			end:  func(w http.ResponseWriter, _ *http.Request) { w.Header().Set(streamEndTrailer, "eof") },
		},
		{
			name: "truncated with reasons",
			end: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(streamErrorCodeTrailer, "stream_truncated")
				w.Header().Set(streamErrorMessageTrailer, "the room stream ended before its end of stream")
				w.Header().Set(streamErrorReasonsTrailer, "source_aborted, stream_gap")
			},
			wantCode: ExitFailed, wantKind: "stream_truncated",
			wantMessage: "the data received is incomplete (reasons: source_aborted, stream_gap).",
		},
		{
			name: "key unavailable mid-stream",
			end: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(streamErrorCodeTrailer, "stream_key_unavailable")
				w.Header().Set(streamErrorMessageTrailer, "the stream channel's MLS key epoch is unavailable")
			},
			wantCode: ExitFailed, wantKind: "stream_key_unavailable", wantMessage: "current encryption key",
		},
		{
			name:     "no trailers",
			end:      func(http.ResponseWriter, *http.Request) {},
			wantCode: ExitFailed, wantKind: "stream_truncated", wantMessage: "without its end-of-stream marker, so the data received is incomplete.",
		},
		{
			name: "agent stopped mid-response",
			end: func(w http.ResponseWriter, _ *http.Request) {
				connection, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = connection.Close()
			},
			wantCode: ExitFailed, wantKind: "stream_truncated", wantMessage: "without its end-of-stream marker",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, stdout := streamListenRunner(t, test.end)
			failure := runner.Execute(context.Background(), streamListenArgs)
			if stdout.String() != "partial" {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if test.wantKind == "" {
				if failure != nil {
					t.Fatalf("failure=%+v", failure)
				}
				return
			}
			if failure == nil || failure.Code != test.wantCode || failure.Kind != test.wantKind || !strings.Contains(failure.Message, test.wantMessage) {
				t.Fatalf("failure=%+v, want code %d kind %q message %q", failure, test.wantCode, test.wantKind, test.wantMessage)
			}
		})
	}
}

// cancelOnWrite cancels a context once output arrives, as a person stopping
// listen does. It has no ReadFrom, so every write goes through Write.
type cancelOnWrite struct {
	output bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	w.cancel()
	return n, err
}

func TestStreamListenStoppedByTheCallerIsNotATruncation(t *testing.T) {
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", streamEndTrailer)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &cancelOnWrite{cancel: cancel}
	runner := Runner{Client: tunnel.NewLocalClient(socket), Out: stdout, Err: &bytes.Buffer{}}
	failure := runner.Execute(ctx, streamListenArgs)
	if stdout.output.String() != "partial" {
		t.Fatalf("stdout=%q", stdout.output.String())
	}
	if failure == nil || failure.Kind != "" || failure.Message != context.Canceled.Error() {
		t.Fatalf("failure=%+v, want the cancellation", failure)
	}
}

// A failed write of the output is reported as such, not as a broken stream.
func TestStreamListenReportsAFailedWrite(t *testing.T) {
	runner, _ := streamListenRunner(t, func(w http.ResponseWriter, _ *http.Request) { w.Header().Set(streamEndTrailer, "eof") })
	runner.Out = failingWriter{}
	failure := runner.Execute(context.Background(), streamListenArgs)
	if failure == nil || failure.Kind != "" || !strings.Contains(failure.Message, "output closed") {
		t.Fatalf("failure=%+v, want the write error", failure)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errOutputClosed }

var errOutputClosed = errors.New("output closed")
