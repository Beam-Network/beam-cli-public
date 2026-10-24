//go:build !windows

package roomcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btr"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/roommanager"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

const (
	objectChannelPath = "/v1/rooms/btr_room_a/channels/btr_channel_files"
	objectPublishPath = objectChannelPath + "/objects/publish"
)

// objectPublishRunner serves an object channel whose publication TTL is
// publicationTTL and records the request paths and the ttl_seconds of every
// publish it receives.
func objectPublishRunner(t *testing.T, publicationTTL time.Duration) (Runner, func() ([]string, []int64)) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	var ttls []int64
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == objectChannelPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"channel": btr.Channel{
				ChannelID: "btr_channel_files", RoomID: "btr_room_a", Name: "files", Kind: btr.ChannelKindObject,
				Limits: btr.ResourceLimits{PublicationTTL: publicationTTL},
			}})
		case r.Method == http.MethodPost && r.URL.Path == objectPublishPath:
			var request struct {
				TTLSeconds int64 `json:"ttl_seconds"`
			}
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			ttls = append(ttls, request.TTLSeconds)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"publication_id": "btr_publication_1", "state": "authorized"})
		default:
			http.NotFound(w, r)
		}
	}))
	runner := Runner{Client: tunnel.NewLocalClient(socket), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	return runner, func() ([]string, []int64) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...), append([]int64(nil), ttls...)
	}
}

func objectPublishArgs(flags ...string) []string {
	args := []string{"tunnel", "room", "--json", "channel", "object", "publish", "btr_room_a", "btr_channel_files", "--file", "./artifact.bin"}
	return append(args, flags...)
}

// The agent refuses a publication that outlives the channel's publication
// TTL, so a publish without --ttl takes the channel's TTL less the clock
// margin, read from the channel before publishing, and an explicit --ttl up to
// it is sent as is.
func TestObjectPublishTTLFollowsTheChannel(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags []string
		want  int64
	}{
		{name: "no --ttl takes the channel publication TTL less the clock margin", want: 55},
		{name: "--ttl below the channel publication TTL", flags: []string{"--ttl", "45"}, want: 45},
		{name: "--ttl equal to the channel publication TTL", flags: []string{"--ttl", "60"}, want: 60},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, received := objectPublishRunner(t, time.Minute)
			if failure := runner.Execute(context.Background(), objectPublishArgs(test.flags...)); failure != nil {
				t.Fatalf("failure = %#v", failure)
			}
			paths, ttls := received()
			if len(paths) != 2 || paths[0] != "GET "+objectChannelPath || paths[1] != "POST "+objectPublishPath {
				t.Fatalf("requests = %q, want the channel read before the publish", paths)
			}
			if len(ttls) != 1 || ttls[0] != test.want {
				t.Fatalf("ttl_seconds = %v, want %d", ttls, test.want)
			}
		})
	}
}

// A --ttl the channel does not allow fails before the publish route is called,
// naming both values and the command that raises the channel's limit.
func TestObjectPublishRefusesTTLAboveTheChannelPublicationTTL(t *testing.T) {
	runner, received := objectPublishRunner(t, time.Minute)
	failure := runner.Execute(context.Background(), objectPublishArgs("--ttl", "300"))
	if failure == nil {
		t.Fatal("publish with --ttl above the channel publication TTL succeeded")
	}
	if failure.Code != ExitUsage || failure.Kind != "object_ttl_exceeds_channel" {
		t.Fatalf("failure = %#v", failure)
	}
	if want := "--ttl 300 is longer than channel btr_channel_files allows: its publication TTL is 60 seconds."; failure.Message != want {
		t.Fatalf("message = %q, want %q", failure.Message, want)
	}
	raise := "`beam room btr_room_a channel btr_channel_files update --name \"files\" --kind object --publication-ttl 300s`"
	if !strings.Contains(failure.Hint, "Omit --ttl to publish within the channel's 60 seconds") || !strings.Contains(failure.Hint, raise) {
		t.Fatalf("hint = %q", failure.Hint)
	}
	if paths, _ := received(); len(paths) != 1 || paths[0] != "GET "+objectChannelPath {
		t.Fatalf("requests = %q, want only the channel read", paths)
	}

	// The suggested update is a valid channel update that sets the TTL asked for.
	value, _, err := (Runner{Err: io.Discard}).channelInput([]string{"--name", "files", "--kind", "object", "--publication-ttl", "300s"}, true)
	if err != nil {
		t.Fatal(err)
	}
	update, ok := value.(roommanager.UpdateChannelInput)
	if !ok || update.Kind != btr.ChannelKindObject || update.Limits.PublicationTTLSeconds != 300 {
		t.Fatalf("update = %#v", value)
	}
}

func TestObjectPublishRefusesNegativeTTL(t *testing.T) {
	runner, received := objectPublishRunner(t, time.Minute)
	failure := runner.Execute(context.Background(), objectPublishArgs("--ttl", "-1"))
	if failure == nil || failure.Code != ExitUsage {
		t.Fatalf("failure = %#v", failure)
	}
	if paths, _ := received(); len(paths) != 0 {
		t.Fatalf("requests = %q, want none", paths)
	}
}

// A default lifetime never drops below half of a short channel TTL.
func TestObjectPublicationTTLDefaultOnShortChannels(t *testing.T) {
	for _, test := range []struct {
		limit time.Duration
		want  int64
	}{
		{limit: time.Minute, want: 55},
		{limit: 10 * time.Second, want: 5},
		{limit: 4 * time.Second, want: 2},
		{limit: time.Second, want: 1},
	} {
		channel := btr.Channel{Limits: btr.ResourceLimits{PublicationTTL: test.limit}}
		got, err := objectPublicationTTL(0, channel)
		if err != nil || got != test.want {
			t.Fatalf("objectPublicationTTL(0, %s) = %d, %v; want %d", test.limit, got, err, test.want)
		}
	}
}
