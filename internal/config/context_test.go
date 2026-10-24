package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNamedContextOverridesBaseConfiguration(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", directory)
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	base := Config{RegistryURL: DefaultRegistryURL, AuthURL: DefaultAuthURL, APIURL: DefaultAPIURL, AgentSocket: paths.AgentSocket, Output: DefaultOutput, CoordinatorURL: "https://base.test"}
	if err := Save(paths, base); err != nil {
		t.Fatal(err)
	}
	dev := base
	dev.CoordinatorURL = "https://dev.test"
	if err := CreateContext(paths, "dev", dev); err != nil {
		t.Fatal(err)
	}
	if err := UseContext(paths, "dev"); err != nil {
		t.Fatal(err)
	}
	loaded, loadedPaths, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CoordinatorURL != "https://dev.test" || loadedPaths.ContextName != "dev" {
		t.Fatalf("loaded=%+v paths=%+v", loaded, loadedPaths)
	}
	if runtime.GOOS != "windows" {
		if mode := mustMode(t, filepath.Join(directory, "contexts", "dev.json")); mode.Perm() != 0o600 {
			t.Fatalf("mode=%o", mode.Perm())
		}
	}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestRequestedContextMustExist(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", directory)
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	base := Config{RegistryURL: DefaultRegistryURL, AuthURL: DefaultAuthURL, APIURL: DefaultAPIURL, AgentSocket: paths.AgentSocket, Output: DefaultOutput, CoordinatorURL: "https://base.test"}
	if err := Save(paths, base); err != nil {
		t.Fatal(err)
	}

	t.Run("flag", func(t *testing.T) {
		_, _, err := LoadContext("missing")
		if err == nil {
			t.Fatal("a missing context must not fall back to the base configuration")
		}
		if !strings.Contains(err.Error(), `context "missing" does not exist`) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("environment", func(t *testing.T) {
		t.Setenv("BEAM_CONTEXT", "missing")
		_, _, err := Load()
		if err == nil || !strings.Contains(err.Error(), `context "missing" does not exist`) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("existing context still loads", func(t *testing.T) {
		dev := base
		dev.CoordinatorURL = "https://dev.test"
		if err := CreateContext(paths, "dev", dev); err != nil {
			t.Fatal(err)
		}
		loaded, loadedPaths, err := LoadContext("dev")
		if err != nil {
			t.Fatal(err)
		}
		if loaded.CoordinatorURL != "https://dev.test" || loadedPaths.ContextName != "dev" {
			t.Fatalf("loaded=%+v paths=%+v", loaded, loadedPaths)
		}
	})
}

// A context file removed behind the CLI's back must not brick every command:
// configuration loads before dispatch, so failing here would also take out
// "beam context list" and "beam context use", the only ways to recover.
func TestActiveContextFileSurvivesAMissingContext(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", directory)
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	base := Config{RegistryURL: DefaultRegistryURL, AuthURL: DefaultAuthURL, APIURL: DefaultAPIURL, AgentSocket: paths.AgentSocket, Output: DefaultOutput, CoordinatorURL: "https://base.test"}
	if err := Save(paths, base); err != nil {
		t.Fatal(err)
	}
	dev := base
	dev.CoordinatorURL = "https://dev.test"
	if err := CreateContext(paths, "dev", dev); err != nil {
		t.Fatal(err)
	}
	if err := UseContext(paths, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(contextPath(paths, "dev")); err != nil {
		t.Fatal(err)
	}
	loaded, loadedPaths, err := Load()
	if err != nil {
		t.Fatalf("a stale active context must still load: %v", err)
	}
	if loaded.CoordinatorURL != "https://base.test" || loadedPaths.ContextName != "dev" {
		t.Fatalf("loaded=%+v paths=%+v", loaded, loadedPaths)
	}
}
