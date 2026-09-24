package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"testing"

	beamupdate "github.com/Beam-Network/beam-cli-public/internal/update"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestUpdateCheckRendersAvailableRelease(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	app := New(version.BuildInfo{Version: "v1.0.0"})
	var received beamupdate.Options
	app.updateRun = func(_ context.Context, options beamupdate.Options) (beamupdate.Result, error) {
		received = options
		return beamupdate.Result{
			CurrentVersion:  options.CurrentVersion,
			Version:         "v1.1.0",
			UpdateAvailable: true,
		}, nil
	}
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"update", "--check"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !received.CheckOnly || received.CurrentVersion != "v1.0.0" || received.BeforeActivate != nil || received.AfterActivate != nil {
		t.Fatalf("options=%#v", received)
	}
	if got := stdout.String(); !strings.Contains(got, "v1.0.0 -> v1.1.0") {
		t.Fatalf("stdout=%q", got)
	}
}

func TestUpdateJSONDescribesBothBinaryUpdate(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	app := New(version.BuildInfo{Version: "v1.0.0"})
	app.updateRun = func(_ context.Context, options beamupdate.Options) (beamupdate.Result, error) {
		return beamupdate.Result{
			CurrentVersion:  options.CurrentVersion,
			Version:         "v1.1.0",
			UpdateAvailable: true,
			Updated:         true,
			InstallDir:      "/opt/beam/bin",
		}, nil
	}
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"--json", "update"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var result beamupdate.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.Version != "v1.1.0" || result.InstallDir != "/opt/beam/bin" {
		t.Fatalf("result=%#v", result)
	}
}

func TestUpdateRejectsUnexpectedArguments(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{
		{"update", "now"},
		{"update", "--check", "later"},
		{"update", "--check", "--force"},
		{"update", "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr)
		if code != ExitUsage {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
	}
}

func TestUpdateHintDistinguishesDiskFullFromPermissions(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"wrapped errno", fmt.Errorf("cannot replace binaries: %w", syscall.ENOSPC), "Free space"},
		{"unix message", fmt.Errorf("cannot replace binaries: no space left on device"), "Free space"},
		{"windows message", fmt.Errorf("cannot replace binaries: There is not enough space on the disk"), "Free space"},
		{"permission", fmt.Errorf("cannot replace binaries: permission denied"), "not writable"},
		{"checksum", fmt.Errorf("bundle checksum mismatch"), "downloaded bundle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := updateHint(test.err); !strings.Contains(got, test.want) {
				t.Fatalf("updateHint(%v) = %q; want %q", test.err, got, test.want)
			}
		})
	}
}

func TestHelpAndCompletionIncludeUpdate(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"help"}, {"help", "update"}, {"completion", "bash"}} {
		var stdout, stderr bytes.Buffer
		code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr)
		if code != ExitOK {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "update") {
			t.Fatalf("args=%v output missing update: %s", args, stdout.String())
		}
	}
}
