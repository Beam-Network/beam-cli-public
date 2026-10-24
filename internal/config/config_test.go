package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestPullRequestPathsAreIsolated(t *testing.T) {
	original := version.Version
	version.Version = "v0.0.0-pr.27.cli.agent"
	t.Cleanup(func() { version.Version = original })
	t.Setenv("BEAM_CONFIG_DIR", "")
	t.Setenv("BEAM_AGENT_SOCKET", "")
	directory := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", directory)
	} else {
		t.Setenv("HOME", directory)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	}

	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(paths.Dir, "beam-pr-27") {
		t.Fatalf("config directory = %q", paths.Dir)
	}
	if runtime.GOOS == "windows" {
		if paths.AgentSocket != `\\.\pipe\beam-agent-pr27` {
			t.Fatalf("agent socket = %q", paths.AgentSocket)
		}
	} else if paths.AgentSocket != filepath.Join(directory, ".beam-pr-27", "agent.sock") {
		t.Fatalf("agent socket = %q", paths.AgentSocket)
	}
}

func TestLoadUsesConfigAndEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", dir)
	t.Setenv("BEAM_REGISTRY_URL", "https://override.example")
	t.Setenv("BEAM_API_URL", "https://api.example")
	t.Setenv("BEAM_OUTPUT", "json")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{
  "registry_url": "https://stored.example",
  "auth_url": "https://auth.example"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, paths, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryURL != "https://override.example" || cfg.AuthURL != "https://auth.example" || cfg.APIURL != "https://api.example" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if paths.Credentials != filepath.Join(dir, "credentials.json") {
		t.Fatalf("credentials path = %q", paths.Credentials)
	}
}

func TestSaveUsesRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not applicable")
	}
	dir := t.TempDir()
	paths := Paths{Dir: dir, Config: filepath.Join(dir, "config.json")}
	cfg := Config{
		RegistryURL: DefaultRegistryURL,
		AuthURL:     DefaultAuthURL,
		APIURL:      DefaultAPIURL,
		AgentSocket: "/tmp/beam.sock",
		Output:      "human",
	}
	if err := Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o, want 600", got)
	}
}

func TestInvalidOutputIsRejected(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_OUTPUT", "xml")
	if _, _, err := Load(); err == nil {
		t.Fatal("expected error")
	}
}

func resolvePathsForVersion(t *testing.T, release string) Paths {
	t.Helper()
	original := version.Version
	version.Version = release
	t.Cleanup(func() { version.Version = original })
	t.Setenv("BEAM_CONFIG_DIR", "")
	t.Setenv("BEAM_AGENT_SOCKET", "")
	directory := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", directory)
	} else {
		t.Setenv("HOME", directory)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	}
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// A production bundle must not read configuration or credentials written by a
// development bundle: config.json outranks the compiled-in endpoints, so a
// shared directory silently points production at the development environment.
func TestDevelopmentAndProductionPathsAreIsolated(t *testing.T) {
	development := resolvePathsForVersion(t, "v0.0.0-dev.cli.agent")
	if !strings.Contains(development.Dir, "beam-dev") {
		t.Fatalf("development config directory = %q", development.Dir)
	}
	if runtime.GOOS == "windows" {
		if development.AgentSocket != `\\.\pipe\beam-agent-dev` {
			t.Fatalf("development agent socket = %q", development.AgentSocket)
		}
	} else if !strings.Contains(development.AgentSocket, ".beam-dev") {
		t.Fatalf("development agent socket = %q", development.AgentSocket)
	}

	production := resolvePathsForVersion(t, "v0.1.0")
	if strings.Contains(filepath.Base(production.Dir), "-dev") {
		t.Fatalf("production config directory = %q", production.Dir)
	}
	if runtime.GOOS == "windows" && production.AgentSocket != `\\.\pipe\beam-agent` {
		t.Fatalf("production agent socket = %q", production.AgentSocket)
	}
	if filepath.Base(development.Dir) == filepath.Base(production.Dir) {
		t.Fatalf("channels share the config directory name %q", filepath.Base(production.Dir))
	}
}
