package version

import "testing"

func TestReleaseBinaryNames(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	tests := []struct {
		version string
		cli     string
		agent   string
	}{
		{version: "dev", cli: "beam-dev", agent: "beam-tunnel-agent-dev"},
		{version: "v0.0.0-dev.abc.def", cli: "beam-dev", agent: "beam-tunnel-agent-dev"},
		{version: "v0.0.0-pr.27.abc.def", cli: "beam-pr-27", agent: "beam-tunnel-agent-pr27"},
		{version: "v1.2.3", cli: "beam", agent: "beam-tunnel-agent"},
	}
	for _, test := range tests {
		Version = test.version
		if got := CLIName(); got != test.cli {
			t.Errorf("CLIName() with %q = %q, want %q", test.version, got, test.cli)
		}
		if got := AgentName(); got != test.agent {
			t.Errorf("AgentName() with %q = %q, want %q", test.version, got, test.agent)
		}
	}
}

func TestPullRequestReleaseIdentity(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	Version = "v0.0.0-pr.27.abcdef123456.123456abcdef"
	if number, ok := PullRequestNumber(); !ok || number != 27 {
		t.Fatalf("PullRequestNumber() = %d, %t", number, ok)
	}
	if !Development() {
		t.Fatal("PR bundle must use development service defaults")
	}
	if got := StateNamespace(); got != "beam-pr-27" {
		t.Fatalf("StateNamespace() = %q", got)
	}
	if got := ChannelManifest(); got != "pr-27.json" {
		t.Fatalf("ChannelManifest() = %q", got)
	}

	for _, invalid := range []string{"v1.2.3", "v0.0.0-pr.0.a.b", "v0.0.0-pr.nope.a.b"} {
		Version = invalid
		if _, ok := PullRequestNumber(); ok {
			t.Errorf("PullRequestNumber() accepted %q", invalid)
		}
	}
}

func TestStateNamespaceIsolatesEveryReleaseChannel(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	for _, test := range []struct {
		version   string
		namespace string
	}{
		{version: "", namespace: "beam"},
		{version: "dev", namespace: "beam-dev"},
		{version: "dev.abcdef", namespace: "beam-dev"},
		{version: "v0.0.0-dev.cli.agent", namespace: "beam-dev"},
		{version: "v1.2.3-dev", namespace: "beam-dev"},
		{version: "v0.0.0-pr.27.cli.agent", namespace: "beam-pr-27"},
		{version: "v1.2.3", namespace: "beam"},
		{version: "v0.1.0", namespace: "beam"},
		{version: "v1.2.3-rc.1", namespace: "beam"},
	} {
		Version = test.version
		if got := StateNamespace(); got != test.namespace {
			t.Errorf("StateNamespace() with %q = %q, want %q", test.version, got, test.namespace)
		}
	}
}

// A development bundle must never read the production configuration: the two
// target different Beam environments, and a shared config.json outranks the
// compiled-in endpoints of whichever channel reads it second.
func TestDevelopmentAndProductionDoNotShareState(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	Version = "v0.0.0-dev.cli.agent"
	development := StateNamespace()
	Version = "v0.1.0"
	production := StateNamespace()

	if development == production {
		t.Fatalf("development and production share the namespace %q", development)
	}
}

// The namespace names each channel's state, so it must agree with the binary
// the user actually runs.
func TestStateNamespaceMatchesCLIName(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	for _, version := range []string{"dev", "v0.0.0-dev.cli.agent", "v0.0.0-pr.27.cli.agent", "v1.2.3"} {
		Version = version
		if got, want := StateNamespace(), CLIName(); got != want {
			t.Errorf("StateNamespace() with %q = %q, want CLIName() %q", version, got, want)
		}
	}
}
