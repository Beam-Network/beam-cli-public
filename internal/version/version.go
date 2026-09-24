package version

import (
	"runtime/debug"
	"strconv"
	"strings"
)

var (
	Version = ""
	Commit  = "none"
	Date    = "unknown"
)

type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func Info() BuildInfo {
	info := BuildInfo{Version: Version, Commit: Commit, Date: Date}
	if Version != "" {
		return info
	}
	build, ok := debug.ReadBuildInfo()
	if ok && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	return info
}

// Development reports whether this build belongs to the development release
// channel. Source builds are production; development and PR bundles set
// Version through ldflags.
func Development() bool {
	if _, ok := PullRequestNumber(); ok {
		return true
	}
	plain := strings.TrimPrefix(strings.TrimSpace(Version), "v")
	return plain == "dev" || strings.HasPrefix(plain, "dev.") ||
		strings.Contains(plain, "-dev.") || strings.HasSuffix(plain, "-dev")
}

func CLIName() string {
	if number, ok := PullRequestNumber(); ok {
		return "beam-pr-" + strconv.Itoa(number)
	}
	if Development() {
		return "beam-dev"
	}
	return "beam"
}

func AgentName() string {
	if number, ok := PullRequestNumber(); ok {
		return "beam-tunnel-agent-pr" + strconv.Itoa(number)
	}
	if Development() {
		return "beam-tunnel-agent-dev"
	}
	return "beam-tunnel-agent"
}

// PullRequestNumber identifies PR bundles whose versions use the
// v0.0.0-pr.<number>.<cli commit>.<agent commit> convention.
func PullRequestNumber() (int, bool) {
	plain := strings.TrimPrefix(strings.TrimSpace(Version), "v")
	marker := strings.Index(plain, "-pr.")
	if marker < 0 {
		return 0, false
	}
	value := plain[marker+len("-pr."):]
	if end := strings.IndexByte(value, '.'); end >= 0 {
		value = value[:end]
	}
	number, err := strconv.Atoi(value)
	return number, err == nil && number > 0
}

// StateNamespace keeps every release channel's machine-local state isolated.
// Development and production target different Beam environments, so sharing a
// namespace would let one channel's configuration, credentials and enrolment
// silently govern the other.
func StateNamespace() string {
	if number, ok := PullRequestNumber(); ok {
		return "beam-pr-" + strconv.Itoa(number)
	}
	if Development() {
		return "beam-dev"
	}
	return "beam"
}

// ChannelManifest is the mutable CDN pointer followed by this bundle's update
// command. PR bundles never promote or consume the dev latest.json pointer.
func ChannelManifest() string {
	if number, ok := PullRequestNumber(); ok {
		return "pr-" + strconv.Itoa(number) + ".json"
	}
	return "latest.json"
}

func ExecutableName(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}
