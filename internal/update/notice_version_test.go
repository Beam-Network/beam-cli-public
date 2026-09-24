package update

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSupersedesOrdersProductionVersions(t *testing.T) {
	cases := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		// The reported defect: a cached verdict written before the user updated.
		{"cached older release stays silent", "v0.1.2", "v0.1.3", false},
		{"same version stays silent", "v0.1.3", "v0.1.3", false},
		{"newer patch notifies", "v0.1.4", "v0.1.3", true},
		{"newer minor notifies", "v0.2.0", "v0.1.9", true},
		{"newer major notifies", "v1.0.0", "v0.9.9", true},
		{"older minor stays silent", "v0.1.9", "v0.2.0", false},
		{"older major stays silent", "v0.9.9", "v1.0.0", false},
		{"double digit patch beats single", "v0.1.10", "v0.1.9", true},
		{"single digit patch loses to double", "v0.1.9", "v0.1.10", false},
		{"missing v prefix is tolerated", "0.1.4", "v0.1.3", true},
		{"release supersedes its prerelease", "v0.1.3", "v0.1.3-rc.1", true},
		{"prerelease does not supersede its release", "v0.1.3-rc.1", "v0.1.3", false},
		{"later prerelease notifies", "v0.1.3-rc.2", "v0.1.3-rc.1", true},
		{"earlier prerelease stays silent", "v0.1.3-rc.1", "v0.1.3-rc.2", false},
		{"numeric prerelease ranks below alphanumeric", "v0.1.3-beta", "v0.1.3-1", true},
		{"build metadata is ignored", "v0.1.3+build.9", "v0.1.3", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := supersedes(testCase.latest, testCase.current); got != testCase.want {
				t.Fatalf("supersedes(%q, %q) = %v, want %v", testCase.latest, testCase.current, got, testCase.want)
			}
		})
	}
}

// Development and pull request bundles are all 0.0.0 and differ only by the
// commit pair they were built from, so precedence cannot order them and any
// difference means a new bundle.
func TestSupersedesTreatsDevelopmentBundlesAsUnordered(t *testing.T) {
	older := "v0.0.0-dev.df443b1ba540.8801800250fd"
	newer := "v0.0.0-dev.321fcb8e2738.8194f7ae3e6f"
	if !supersedes(newer, older) {
		t.Fatal("a different development bundle must notify")
	}
	// Lexically the reverse direction sorts the other way; both must still
	// notify, because neither ordering is meaningful.
	if !supersedes(older, newer) {
		t.Fatal("development bundles carry no ordering, so any difference must notify")
	}
	if supersedes(newer, newer) {
		t.Fatal("an identical development bundle must stay silent")
	}
	prBundle := "v0.0.0-pr.42.abcdef123456.abcdef123456"
	if !supersedes(prBundle, older) {
		t.Fatal("a pull request bundle differing from the running one must notify")
	}
}

// A production release must always win against a development bundle rather than
// being compared as 0.0.0.
func TestSupersedesAcrossChannelShapes(t *testing.T) {
	if !supersedes("v0.1.3", "v0.0.0-dev.df443b1ba540.8801800250fd") {
		t.Fatal("a release offered to a development build must notify")
	}
	if !supersedes("v0.0.0-dev.df443b1ba540.8801800250fd", "v0.1.3") {
		t.Fatal("an unordered bundle differing from the running release must notify")
	}
}

func TestSupersedesHandlesEmptyAndMalformedVersions(t *testing.T) {
	cases := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		{"no channel version stays silent", "", "v0.1.3", false},
		{"unknown running version notifies", "v0.1.3", "", true},
		{"both empty stays silent", "", "", false},
		// An unparseable version must not be silently treated as older, or a
		// real update could be withheld.
		{"malformed channel version notifies", "not-a-version", "v0.1.3", true},
		{"malformed running version notifies", "v0.1.3", "not-a-version", true},
		{"identical malformed stays silent", "not-a-version", "not-a-version", false},
		{"truncated version notifies", "v0.1", "v0.1.3", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := supersedes(testCase.latest, testCase.current); got != testCase.want {
				t.Fatalf("supersedes(%q, %q) = %v, want %v", testCase.latest, testCase.current, got, testCase.want)
			}
		})
	}
}

func TestParseVersionRejectsNonNumericCore(t *testing.T) {
	for _, value := range []string{"v0.1.x", "0.1", "0.1.2.3", "", "v", "-1.2.3", "0.1.3-"} {
		if _, ok := parseVersion(value); ok {
			t.Fatalf("parseVersion(%q) reported success", value)
		}
	}
	for _, value := range []string{"0.1.3", "v0.1.3", "v10.20.30", "v0.1.3-rc.1", "v0.1.3+build"} {
		if _, ok := parseVersion(value); !ok {
			t.Fatalf("parseVersion(%q) failed", value)
		}
	}
}

// The reported failure, end to end: a cache written while the channel served
// v0.1.2, a user who updated to v0.1.3 inside the interval, and therefore no
// refetch. Before the ordering fix this told the user to install v0.1.2.
func TestNoticeDoesNotAdvertiseAnOlderCachedVersion(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	checkedAt := time.Unix(1789014327, 0)
	writeNoticeCache(cachePath, noticeCache{CheckedAt: checkedAt.Unix(), LatestVersion: "v0.1.2"})

	notice := Notice(context.Background(), NoticeOptions{
		CachePath: cachePath,
		// Deliberately unreachable: the cache is still fresh, so no request
		// should be made, and the verdict must come from the cached value.
		BaseURL:         "http://127.0.0.1:1",
		ChannelManifest: "latest.json",
		CurrentVersion:  "v0.1.3",
		Interval:        24 * time.Hour,
		Now:             func() time.Time { return checkedAt.Add(time.Hour) },
	})
	if notice != "" {
		t.Fatalf("Notice returned %q; a cached older version must not be advertised", notice)
	}
}

func TestNoticeStillReportsANewerCachedVersion(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	checkedAt := time.Unix(1789014327, 0)
	writeNoticeCache(cachePath, noticeCache{CheckedAt: checkedAt.Unix(), LatestVersion: "v0.1.4"})

	notice := Notice(context.Background(), NoticeOptions{
		CachePath:       cachePath,
		BaseURL:         "http://127.0.0.1:1",
		ChannelManifest: "latest.json",
		CurrentVersion:  "v0.1.3",
		Interval:        24 * time.Hour,
		Now:             func() time.Time { return checkedAt.Add(time.Hour) },
	})
	if notice != "v0.1.4" {
		t.Fatalf("Notice returned %q, want v0.1.4", notice)
	}
}
