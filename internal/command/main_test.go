package command

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// TestMain runs the tests with their own home directory, configuration, agent
// socket, and agent state, and with an agent starter that fails the test that
// reaches it. A test that exercises agent startup injects App.daemonStart.
// Credential stores keep refresh tokens in memory, and a store that would use
// the operating-system keyring fails the test that reaches it.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	root, err := os.MkdirTemp("", "beam-command-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(root)
	if err := testutil.IsolateAgentProcess(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	startDaemon = refuseAgentStart
	auth.UseSystemKeyring(testutil.RefusingKeyring{})
	newAuthStore = memoryAuthStore
	return m.Run()
}

// memoryKeyrings holds one in-memory keyring per credentials file, so tests
// with their own configuration directory never see each other's refresh
// tokens.
var memoryKeyrings sync.Map

// memoryAuthStore stands in for auth.NewStore, which keeps the refresh token in
// the developer's keychain.
func memoryAuthStore(path string) auth.Store {
	keyring, _ := memoryKeyrings.LoadOrStore(path, &testutil.MemoryKeyring{})
	return auth.NewStoreWithKeyring(path, keyring.(*testutil.MemoryKeyring))
}

// refuseAgentStart stands in for tunnel.StartDaemon, which would start the
// agent installed on this machine.
func refuseAgentStart(_ context.Context, options tunnel.StartOptions) (tunnel.StartResult, error) {
	panic(fmt.Sprintf("test tried to start the installed %s for socket %s; inject App.daemonStart", version.AgentName(), options.SocketPath))
}

// agentNotInstalled starts no agent and fails as tunnel.StartDaemon does on a
// machine without the agent.
func agentNotInstalled(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
	return tunnel.StartResult{}, fmt.Errorf("%s companion binary was not found", version.AgentName())
}
