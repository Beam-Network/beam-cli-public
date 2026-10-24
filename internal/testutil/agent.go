package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// agentInstallationVariables name an installed agent binary or log, or move
// the configuration out of the home directory. Isolated tests run without
// them.
var agentInstallationVariables = []string{
	"BEAM_TUNNEL_AGENT_BINARY", "BEAM_AGENTD_BINARY", "BEAM_AGENT_BINARY", "BEAM_AGENT_LOG", "XDG_CONFIG_HOME",
}

// agentLocations maps the variables that place the home directory, the
// configuration, the agent socket, and the agent state to directories under
// root, creating the directories.
func agentLocations(root string) (map[string]string, error) {
	locations := map[string]string{
		"HOME":                         filepath.Join(root, "home"),
		"USERPROFILE":                  filepath.Join(root, "home"),
		"BEAM_CONFIG_DIR":              filepath.Join(root, "config"),
		"BEAM_TUNNEL_DAEMON_STATE_DIR": filepath.Join(root, "state"),
	}
	for _, directory := range locations {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	locations["BEAM_AGENT_SOCKET"] = filepath.Join(root, "agent.sock")
	return locations, nil
}

// IsolateAgentProcess points the home directory, configuration, agent socket,
// and agent state of the whole test process at directories under root, and
// removes the variables that name an installed agent binary or log, so no test
// in the process reads or writes the developer's Beam installation. PATH is
// kept for tools such as Node.js; IsolateAgent also replaces it.
func IsolateAgentProcess(root string) error {
	locations, err := agentLocations(root)
	if err != nil {
		return err
	}
	for name, value := range locations {
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	for _, name := range agentInstallationVariables {
		if err := os.Unsetenv(name); err != nil {
			return err
		}
	}
	return nil
}

// IsolateAgent gives t its own home directory, configuration, agent state, and
// agent socket, where no agent listens, and a PATH holding no agent binary. It
// returns the socket path.
func IsolateAgent(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	locations, err := agentLocations(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range locations {
		t.Setenv(name, value)
	}
	for _, name := range agentInstallationVariables {
		t.Setenv(name, "")
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return locations["BEAM_AGENT_SOCKET"]
}
