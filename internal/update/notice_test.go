package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func manifestServer(t *testing.T, version string, hits *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 1, "version": version})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestNoticeReportsANewerVersion(t *testing.T) {
	hits := 0
	server := manifestServer(t, "v0.2.0", &hits)
	got := Notice(t.Context(), NoticeOptions{
		CachePath:       filepath.Join(t.TempDir(), "update-check.json"),
		BaseURL:         server.URL,
		ChannelManifest: "latest.json",
		CurrentVersion:  "v0.1.1",
	})
	if got != "v0.2.0" {
		t.Fatalf("Notice() = %q, want v0.2.0", got)
	}
}

func TestNoticeIsSilentOnTheCurrentVersion(t *testing.T) {
	hits := 0
	server := manifestServer(t, "v0.1.1", &hits)
	if got := Notice(t.Context(), NoticeOptions{
		CachePath:       filepath.Join(t.TempDir(), "update-check.json"),
		BaseURL:         server.URL,
		ChannelManifest: "latest.json",
		CurrentVersion:  "v0.1.1",
	}); got != "" {
		t.Fatalf("Notice() = %q, want empty", got)
	}
}

// The whole point of the cache is that ordinary commands pay nothing. A second
// call inside the interval must not reach the network.
func TestNoticeDoesNotRefetchWithinTheInterval(t *testing.T) {
	hits := 0
	server := manifestServer(t, "v0.2.0", &hits)
	cache := filepath.Join(t.TempDir(), "update-check.json")
	options := NoticeOptions{
		CachePath:       cache,
		BaseURL:         server.URL,
		ChannelManifest: "latest.json",
		CurrentVersion:  "v0.1.1",
	}
	for i := 0; i < 5; i++ {
		if got := Notice(t.Context(), options); got != "v0.2.0" {
			t.Fatalf("call %d: Notice() = %q, want v0.2.0", i, got)
		}
	}
	if hits != 1 {
		t.Fatalf("manifest fetched %d times, want 1", hits)
	}
}

func TestNoticeRefetchesAfterTheInterval(t *testing.T) {
	hits := 0
	server := manifestServer(t, "v0.2.0", &hits)
	cache := filepath.Join(t.TempDir(), "update-check.json")
	base := time.Now()
	options := NoticeOptions{
		CachePath:       cache,
		BaseURL:         server.URL,
		ChannelManifest: "latest.json",
		CurrentVersion:  "v0.1.1",
		Interval:        time.Hour,
		Now:             func() time.Time { return base },
	}
	Notice(t.Context(), options)
	options.Now = func() time.Time { return base.Add(2 * time.Hour) }
	Notice(t.Context(), options)
	if hits != 2 {
		t.Fatalf("manifest fetched %d times, want 2", hits)
	}
}

// An unreachable CDN must be silent, and must not make every subsequent command
// retry it.
func TestNoticeSurvivesAnUnreachableChannel(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "update-check.json")
	options := NoticeOptions{
		CachePath:       cache,
		BaseURL:         "http://127.0.0.1:1",
		ChannelManifest: "latest.json",
		CurrentVersion:  "v0.1.1",
	}
	if got := Notice(t.Context(), options); got != "" {
		t.Fatalf("Notice() = %q, want empty on failure", got)
	}
	payload, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("cache was not written after a failed check: %v", err)
	}
	var decoded noticeCache
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.CheckedAt == 0 {
		t.Fatalf("failed check did not record a timestamp: %s", payload)
	}
}

func TestNoticeWithoutACachePathDoesNothing(t *testing.T) {
	hits := 0
	server := manifestServer(t, "v0.2.0", &hits)
	if got := Notice(t.Context(), NoticeOptions{BaseURL: server.URL, ChannelManifest: "latest.json", CurrentVersion: "v0.1.1"}); got != "" {
		t.Fatalf("Notice() = %q, want empty", got)
	}
	if hits != 0 {
		t.Fatalf("fetched %d times without a cache path, want 0", hits)
	}
}
