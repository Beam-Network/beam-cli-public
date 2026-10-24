package tunnel

import (
	"slices"
	"testing"
)

func TestDaemonEnvironmentDropsCLIOnlySecrets(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"BEAM_API_KEY=b1m_secret",
		"beam_api_key=b1m_secret_lowercase",
		"BEAM_API_KEY_FILE=/kept/not/a/secret/name",
		"BEAM_API_URL=https://api.example",
		"BEAM_AGENT_API_KEY=kept_for_the_agent",
		"=C:=C:\\",
	}
	got := daemonEnvironment(environ)
	want := []string{
		"PATH=/usr/bin",
		"BEAM_API_KEY_FILE=/kept/not/a/secret/name",
		"BEAM_API_URL=https://api.example",
		"BEAM_AGENT_API_KEY=kept_for_the_agent",
		"=C:=C:\\",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("daemonEnvironment=%q, want %q", got, want)
	}
}
