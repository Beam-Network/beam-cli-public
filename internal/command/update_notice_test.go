package command

import (
	"bytes"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestUpdateNoticeIsSuppressedForMachineReadableOutput(t *testing.T) {
	original := version.Version
	version.Version = "v0.1.1"
	t.Cleanup(func() { version.Version = original })
	t.Setenv("BEAM_NO_UPDATE_NOTICE", "")

	// A notice on a --json or --quiet run would corrupt output a script parses.
	for _, mode := range []output.Mode{output.JSON, output.Quiet} {
		if updateNoticeWanted(output.Renderer{Mode: mode, Err: &bytes.Buffer{}}) {
			t.Errorf("notice allowed in %v mode", mode)
		}
	}
	if !updateNoticeWanted(output.Renderer{Mode: output.Human, Err: &bytes.Buffer{}}) {
		t.Error("notice suppressed in human mode")
	}
}

func TestUpdateNoticeRespectsTheOptOut(t *testing.T) {
	original := version.Version
	version.Version = "v0.1.1"
	t.Cleanup(func() { version.Version = original })

	for _, value := range []string{"1", "true", "yes", "on", "TRUE"} {
		t.Setenv("BEAM_NO_UPDATE_NOTICE", value)
		if updateNoticeWanted(output.Renderer{Mode: output.Human, Err: &bytes.Buffer{}}) {
			t.Errorf("notice allowed with BEAM_NO_UPDATE_NOTICE=%q", value)
		}
	}
	t.Setenv("BEAM_NO_UPDATE_NOTICE", "")
	if !updateNoticeWanted(output.Renderer{Mode: output.Human, Err: &bytes.Buffer{}}) {
		t.Error("notice suppressed with an empty opt-out")
	}
}

// A PR bundle tracks one candidate and a source build has no published
// counterpart, so neither has an update to offer.
func TestUpdateNoticeIsSuppressedWithoutAPublishedChannel(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })
	t.Setenv("BEAM_NO_UPDATE_NOTICE", "")

	for _, test := range []struct {
		version string
		wanted  bool
	}{
		{version: "v0.0.0-pr.27.cli.agent", wanted: false},
		{version: "dev", wanted: false},
		{version: "", wanted: false},
		{version: "v0.0.0-dev.cli.agent", wanted: true},
		{version: "v0.1.1", wanted: true},
	} {
		version.Version = test.version
		if got := updateNoticeWanted(output.Renderer{Mode: output.Human, Err: &bytes.Buffer{}}); got != test.wanted {
			t.Errorf("version %q: updateNoticeWanted() = %t, want %t", test.version, got, test.wanted)
		}
	}
}
