package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
)

// TestMain runs the tests with their own home directory, configuration, agent
// socket, and agent state, and lets them start only the fake agents they wrote.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	root, err := os.MkdirTemp("", "beam-tunnel-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(root)
	if err := testutil.IsolateAgentProcess(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	startAgent = startFakeAgent
	return m.Run()
}

// startFakeAgent starts command only when its binary lies under the temporary
// directory, where tests write fake agents. Any other binary is an installed
// agent, and the test that tried to start it fails.
func startFakeAgent(command *exec.Cmd) error {
	relative, err := filepath.Rel(filepath.Clean(os.TempDir()), filepath.Clean(command.Path))
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		panic(fmt.Sprintf("test tried to start %s, an agent it did not write under %s", command.Path, os.TempDir()))
	}
	return command.Start()
}
