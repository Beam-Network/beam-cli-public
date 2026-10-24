package update

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// NoticeInterval is how long a check is trusted before the channel manifest is
// consulted again. Ordinary commands must not pay for a network round trip, so
// the manifest is read at most once per interval and every other invocation is
// answered from the cache.
const NoticeInterval = 24 * time.Hour

// noticeTimeout bounds the manifest read. The notice is a convenience, so a
// slow or unreachable CDN must cost a command almost nothing.
const noticeTimeout = 2 * time.Second

// NoticeOptions describes one channel's update check.
type NoticeOptions struct {
	// CachePath holds the last check. It lives in the channel's own directory,
	// so a development and a production install never share a verdict.
	CachePath       string
	BaseURL         string
	ChannelManifest string
	CurrentVersion  string
	// CurrentBuildDate is the running bundle's RFC 3339 build time. It orders
	// development and pull request bundles, whose versions carry no precedence.
	CurrentBuildDate string
	Interval         time.Duration
	HTTPClient       *http.Client
	Now              func() time.Time
}

type noticeCache struct {
	CheckedAt         int64  `json:"checked_at"`
	LatestVersion     string `json:"latest_version,omitempty"`
	LatestPublishedAt string `json:"latest_published_at,omitempty"`
}

// bundle identifies a Beam bundle for ordering. built is its RFC 3339 build
// time: a release workflow stamps the same instant into the binaries and into
// the manifest's publishedAt.
type bundle struct {
	version string
	built   string
}

// Notice reports the version a channel is offering when it supersedes the
// running bundle, or an empty string when there is nothing to say. It never
// returns an error: a failed check is indistinguishable to the caller from
// "nothing new", because a convenience notice must not affect a command.
//
// A cached verdict can outlive the update that answers it, so the comparison is
// by precedence rather than difference; otherwise updating inside the interval
// leaves the user being told to install the version they just replaced.
func Notice(ctx context.Context, options NoticeOptions) string {
	if strings.TrimSpace(options.CachePath) == "" {
		return ""
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	interval := options.Interval
	if interval <= 0 {
		interval = NoticeInterval
	}

	cache := readNoticeCache(options.CachePath)
	if now().Unix()-cache.CheckedAt >= int64(interval.Seconds()) {
		latest, err := fetchLatest(ctx, options)
		// The timestamp advances even on failure. Without that, an unreachable
		// CDN would be retried by every single command.
		cache.CheckedAt = now().Unix()
		if err == nil && latest.version != "" {
			cache.LatestVersion = latest.version
			cache.LatestPublishedAt = latest.built
		}
		writeNoticeCache(options.CachePath, cache)
	}

	latest := bundle{version: strings.TrimSpace(cache.LatestVersion), built: strings.TrimSpace(cache.LatestPublishedAt)}
	current := bundle{version: strings.TrimSpace(options.CurrentVersion), built: strings.TrimSpace(options.CurrentBuildDate)}
	if !supersedes(latest, current) {
		return ""
	}
	return latest.version
}

// supersedes reports whether the channel's version is worth telling the user
// about.
//
// Production versions are ordered by semantic version precedence, so a channel
// that is behind the running build says nothing. Testing mere inequality told
// users to downgrade whenever the cached verdict predated their own update, or
// whenever a channel was rolled back.
//
// Development and pull request bundles are 0.0.0 plus the commit pair they were
// built from, which carries no ordering, so two of them are ordered by build
// time instead. Only a bundle built strictly later is worth announcing; when
// either build time is unknown the notice stays silent rather than offer what
// may be an older bundle.
func supersedes(latest, current bundle) bool {
	if latest.version == "" || latest.version == current.version {
		return false
	}
	if current.version == "" {
		return true
	}
	latestParts, latestOK := parseVersion(latest.version)
	currentParts, currentOK := parseVersion(current.version)
	if !latestOK || !currentOK {
		return true
	}
	if latestParts.unordered() && currentParts.unordered() {
		return builtLater(latest.built, current.built)
	}
	if latestParts.unordered() || currentParts.unordered() {
		return true
	}
	return latestParts.compare(currentParts) > 0
}

// builtLater reports whether build time latest is strictly after current. An
// absent or unparseable time orders nothing.
func builtLater(latest, current string) bool {
	latestTime, latestErr := time.Parse(time.RFC3339, latest)
	currentTime, currentErr := time.Parse(time.RFC3339, current)
	if latestErr != nil || currentErr != nil {
		return false
	}
	return latestTime.After(currentTime)
}

type semanticVersion struct {
	major, minor, patch uint64
	prerelease          []string
}

// unordered reports a bundle whose version conveys no precedence. Development
// and pull request builds are all 0.0.0 and differ only in a commit pair, so
// comparing them by precedence would be meaningless.
func (v semanticVersion) unordered() bool {
	return v.major == 0 && v.minor == 0 && v.patch == 0 && len(v.prerelease) > 0
}

func (v semanticVersion) compare(other semanticVersion) int {
	for _, pair := range [][2]uint64{{v.major, other.major}, {v.minor, other.minor}, {v.patch, other.patch}} {
		if pair[0] != pair[1] {
			if pair[0] > pair[1] {
				return 1
			}
			return -1
		}
	}
	// A release supersedes any prerelease of the same version, and a prerelease
	// never supersedes the release it leads to.
	switch {
	case len(v.prerelease) == 0 && len(other.prerelease) == 0:
		return 0
	case len(v.prerelease) == 0:
		return 1
	case len(other.prerelease) == 0:
		return -1
	}
	for i := 0; i < len(v.prerelease) && i < len(other.prerelease); i++ {
		if result := comparePrereleaseIdentifier(v.prerelease[i], other.prerelease[i]); result != 0 {
			return result
		}
	}
	switch {
	case len(v.prerelease) > len(other.prerelease):
		return 1
	case len(v.prerelease) < len(other.prerelease):
		return -1
	}
	return 0
}

// comparePrereleaseIdentifier follows semantic version precedence: numeric
// identifiers compare numerically and rank below alphanumeric ones, which
// compare lexically.
func comparePrereleaseIdentifier(left, right string) int {
	leftNumber, leftNumeric := parseUint(left)
	rightNumber, rightNumeric := parseUint(right)
	switch {
	case leftNumeric && rightNumeric:
		switch {
		case leftNumber > rightNumber:
			return 1
		case leftNumber < rightNumber:
			return -1
		}
		return 0
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	case left > right:
		return 1
	case left < right:
		return -1
	}
	return 0
}

func parseVersion(value string) (semanticVersion, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "v")
	if trimmed == "" {
		return semanticVersion{}, false
	}
	core := trimmed
	var prerelease []string
	// Build metadata does not affect precedence.
	if index := strings.IndexByte(core, '+'); index >= 0 {
		core = core[:index]
	}
	if index := strings.IndexByte(core, '-'); index >= 0 {
		prerelease = strings.Split(core[index+1:], ".")
		core = core[:index]
	}
	fields := strings.Split(core, ".")
	if len(fields) != 3 {
		return semanticVersion{}, false
	}
	parsed := semanticVersion{prerelease: prerelease}
	numbers := [3]*uint64{&parsed.major, &parsed.minor, &parsed.patch}
	for i, field := range fields {
		number, ok := parseUint(field)
		if !ok {
			return semanticVersion{}, false
		}
		*numbers[i] = number
	}
	for _, identifier := range prerelease {
		if identifier == "" {
			return semanticVersion{}, false
		}
	}
	return parsed, true
}

func parseUint(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

func fetchLatest(ctx context.Context, options NoticeOptions) (bundle, error) {
	base := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	name := strings.TrimSpace(options.ChannelManifest)
	if base == "" || name == "" {
		return bundle{}, nil
	}
	timedCtx, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(timedCtx, http.MethodGet, base+"/"+name, nil)
	if err != nil {
		return bundle{}, err
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: noticeTimeout}
	}
	response, err := client.Do(request)
	if err != nil {
		return bundle{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return bundle{}, nil
	}
	var decoded manifest
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return bundle{}, err
	}
	return bundle{version: strings.TrimSpace(decoded.Version), built: strings.TrimSpace(decoded.PublishedAt)}, nil
}

func readNoticeCache(path string) noticeCache {
	payload, err := os.ReadFile(path)
	if err != nil {
		return noticeCache{}
	}
	var cache noticeCache
	if err := json.Unmarshal(payload, &cache); err != nil {
		return noticeCache{}
	}
	return cache
}

func writeNoticeCache(path string, cache noticeCache) {
	payload, err := json.Marshal(cache)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// A failed write is ignored: the next command simply checks again.
	_ = os.WriteFile(path, payload, 0o600)
}
