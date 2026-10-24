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

func StateNamespace() string {
	if number, ok := PullRequestNumber(); ok {
		return "beam-pr-" + strconv.Itoa(number)
	}
	if Development() {
		return "beam-dev"
	}
	return "beam"
}

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
