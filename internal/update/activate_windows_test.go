//go:build windows

package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const windowsActivateHelper = "BEAM_TEST_WINDOWS_ACTIVATE_HELPER"

func TestWindowsDeferredActivationReplacesBothBinaries(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	install := filepath.Join(root, "install")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(install, 0o700); err != nil {
		t.Fatal(err)
	}
	cliName := version.ExecutableName(version.CLIName(), "windows")
	agentName := version.ExecutableName(version.AgentName(), "windows")
	cliPath := filepath.Join(install, cliName)
	agentPath := filepath.Join(install, agentName)
	for path, contents := range map[string]string{
		filepath.Join(source, cliName):   "new-cli",
		filepath.Join(source, agentName): "new-agent",
		cliPath:                          "old-cli",
		agentPath:                        "old-agent",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsActivationHelperProcess$")
	command.Env = append(os.Environ(), windowsActivateHelper+"=1", "BEAM_TEST_WINDOWS_SOURCE="+source, "BEAM_TEST_WINDOWS_CLI="+cliPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("activation helper: %v: %s", err, output)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cli, cliErr := os.ReadFile(cliPath)
		agent, agentErr := os.ReadFile(agentPath)
		_, sourceErr := os.Stat(source)
		if cliErr == nil && agentErr == nil && string(cli) == "new-cli" && string(agent) == "new-agent" && os.IsNotExist(sourceErr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if data, err := os.ReadFile(filepath.Join(install, ".beam-update-error.log")); err == nil {
		t.Fatalf("deferred activation failed: %s", data)
	}
	t.Fatal("deferred activation did not replace both binaries")
}

func TestWindowsActivationHelperProcess(t *testing.T) {
	if os.Getenv(windowsActivateHelper) != "1" {
		return
	}
	deferred, preserve, err := activate(os.Getenv("BEAM_TEST_WINDOWS_SOURCE"), os.Getenv("BEAM_TEST_WINDOWS_CLI"), "windows")
	if err != nil || !deferred || !preserve {
		t.Fatalf("activate() = deferred %t, preserve %t, err %v", deferred, preserve, err)
	}
}
