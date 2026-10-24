//go:build !windows

package roomcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btr"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

const objectStatusPath = objectChannelPath + "/objects/btr_pub_1/status"

func objectPublicationStatus(state, reason string) map[string]any {
	transfer := map[string]any{"status": state}
	if reason != "" {
		transfer["error_message"] = reason
	}
	return map[string]any{"publication_id": "btr_pub_1", "status": map[string]any{"publisher": map[string]any{
		"state": state, "room_transfer": transfer,
		"deliveries": []any{
			map[string]any{"member_id": "btr_member_a", "state": "delivered"},
			map[string]any{"member_id": "btr_member_b", "state": "failed", "unavailable_reason": "agent_offline"},
		},
	}}}
}

// objectMissedSentence is how a failure names the recipient objectPublicationStatus
// did not deliver to.
const objectMissedSentence = " It failed for 1 of 2 recipients: btr_member_b (agent_offline)."

// objectFollowRunner serves an object channel whose publish answers with
// published and whose status route is served by status. It returns the
// runner and the requests it received.
func objectFollowRunner(t *testing.T, published map[string]any, status http.HandlerFunc) (Runner, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == objectChannelPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"channel": btr.Channel{
				ChannelID: "btr_channel_files", RoomID: "btr_room_a", Name: "files", Kind: btr.ChannelKindObject,
				Limits: btr.ResourceLimits{PublicationTTL: time.Minute},
			}})
		case r.Method == http.MethodPost && r.URL.Path == objectPublishPath:
			_ = json.NewEncoder(w).Encode(published)
		case r.Method == http.MethodGet && r.URL.Path == objectStatusPath && status != nil:
			status(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	runner := Runner{Client: tunnel.NewLocalClient(socket), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	return runner, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
}

func countRequests(requests []string, request string) int {
	count := 0
	for _, candidate := range requests {
		if candidate == request {
			count++
		}
	}
	return count
}

// A followed publish that ends in any state but completed fails once, as an
// operation failure naming the outcome, the coordinator's reason and each
// recipient that missed the file with its reason, after exactly one publish
// request. --allow-partial accepts a partial delivery.
func TestFollowedObjectPublishFailsWithItsOutcome(t *testing.T) {
	const reason = "destination_access_denied: The destination storage refused Beam's requests (403 AccessDenied)."
	for _, test := range []struct {
		state        string
		allowPartial bool
		wantKind     string
		wantMessage  string
	}{
		{state: "partial", wantKind: "object_publication_partial", wantMessage: "Room object publication btr_pub_1 was only partially delivered: " + reason + objectMissedSentence},
		{state: "failed", wantKind: "object_publication_failed", wantMessage: "Room object publication btr_pub_1 failed: " + reason + objectMissedSentence},
		{state: "cancelled", wantKind: "object_publication_cancelled", wantMessage: "Room object publication btr_pub_1 was cancelled: " + reason + objectMissedSentence},
		{state: "expired", wantKind: "object_publication_expired", wantMessage: "Room object publication btr_pub_1 expired: " + reason + objectMissedSentence},
		{state: "rejected", wantKind: "object_publication_rejected", wantMessage: "Room object publication btr_pub_1 was rejected: " + reason + objectMissedSentence},
		{state: "partial", allowPartial: true},
		{state: "completed"},
	} {
		name := test.state
		if test.allowPartial {
			name += " with --allow-partial"
		}
		t.Run(name, func(t *testing.T) {
			runner, received := objectFollowRunner(t, objectPublicationStatus(test.state, reason), nil)
			flags := []string{"--follow"}
			if test.allowPartial {
				flags = append(flags, "--allow-partial")
			}
			failure := runner.Execute(context.Background(), objectPublishArgs(flags...))
			if count := countRequests(received(), "POST "+objectPublishPath); count != 1 {
				t.Fatalf("publish requests = %d, want 1", count)
			}
			if test.wantKind == "" {
				if failure != nil {
					t.Fatalf("failure = %#v, want success", failure)
				}
				return
			}
			if failure == nil || failure.Code != ExitFailed || failure.Kind != test.wantKind || failure.Message != test.wantMessage {
				t.Fatalf("failure = %#v", failure)
			}
			if strings.Contains(strings.ToLower(failure.Hint), "agent") {
				t.Fatalf("hint = %q mentions the agent", failure.Hint)
			}
		})
	}
}

// The outcome is read from the status the follow polls once the publish
// answered with a publication still in progress.
func TestFollowedObjectPublishFailsWithThePolledOutcome(t *testing.T) {
	runner, received := objectFollowRunner(t, objectPublicationStatus("in_progress", ""), func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(objectPublicationStatus("partial", ""))
	})
	failure := runner.Execute(context.Background(), objectPublishArgs("--follow", "--interval", "250ms"))
	if failure == nil || failure.Code != ExitFailed || failure.Kind != "object_publication_partial" ||
		failure.Message != "Room object publication btr_pub_1 was only partially delivered."+objectMissedSentence {
		t.Fatalf("failure = %#v", failure)
	}
	requests := received()
	if countRequests(requests, "POST "+objectPublishPath) != 1 || countRequests(requests, "GET "+objectStatusPath) != 1 {
		t.Fatalf("requests = %q, want one publish and one status read", requests)
	}
}

// Once the publication was submitted, losing the agent while following it is
// final too: reporting the agent unavailable would make beam start it and
// publish the file a second time.
func TestFollowedObjectPublishLosingTheAgentIsFinal(t *testing.T) {
	runner, received := objectFollowRunner(t, objectPublicationStatus("in_progress", ""), func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	})
	failure := runner.Execute(context.Background(), objectPublishArgs("--follow", "--interval", "250ms"))
	if failure == nil || failure.Code != ExitFailed || failure.Kind != "object_publication_follow_failed" {
		t.Fatalf("failure = %#v", failure)
	}
	if !strings.Contains(failure.Message, "btr_pub_1 was submitted") {
		t.Fatalf("message = %q", failure.Message)
	}
	if !strings.Contains(failure.Hint, "`beam room btr_room_a channel btr_channel_files object watch btr_pub_1`") {
		t.Fatalf("hint = %q, want the command that follows the publication", failure.Hint)
	}
	if count := countRequests(received(), "POST "+objectPublishPath); count != 1 {
		t.Fatalf("publish requests = %d, want 1", count)
	}
}

// A publish whose connection broke after the agent read it may have created a
// publication, so it is final and the hint says how to find it.
func TestObjectPublishInterruptedAfterConnectingIsFinal(t *testing.T) {
	var mu sync.Mutex
	publishes := 0
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == objectChannelPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"channel": btr.Channel{
				ChannelID: "btr_channel_files", RoomID: "btr_room_a", Kind: btr.ChannelKindObject,
				Limits: btr.ResourceLimits{PublicationTTL: time.Minute},
			}})
		case r.Method == http.MethodPost && r.URL.Path == objectPublishPath:
			mu.Lock()
			publishes++
			mu.Unlock()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	runner := Runner{Client: tunnel.NewLocalClient(socket), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	failure := runner.Execute(context.Background(), objectPublishArgs("--follow"))
	if failure == nil || failure.Code != ExitFailed || failure.Kind != "agent_request_interrupted" {
		t.Fatalf("failure = %#v", failure)
	}
	if !strings.Contains(failure.Hint, "`beam room btr_room_a channel btr_channel_files object list`") {
		t.Fatalf("hint = %q, want the command that lists the channel's publications", failure.Hint)
	}
	mu.Lock()
	defer mu.Unlock()
	if publishes != 1 {
		t.Fatalf("publishes = %d, want 1", publishes)
	}
}

// An agent that is not running is still reported unavailable, so beam starts
// it and runs the command; nothing was published.
func TestObjectPublishWithoutAnAgentReportsItUnavailable(t *testing.T) {
	runner := Runner{Client: tunnel.NewLocalClient(filepath.Join(t.TempDir(), "missing.sock")), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	failure := runner.Execute(context.Background(), objectPublishArgs("--follow"))
	if failure == nil || failure.Code != ExitDaemon {
		t.Fatalf("failure = %#v, want code %d", failure, ExitDaemon)
	}
}
