//go:build !windows

package update

import (
	"context"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestRunReplacesBeamAndAgentOnlyAfterVerification(t *testing.T) {
	archive := testTarGzip(t, map[string]string{version.CLIName(): "new beam", version.AgentName(): "new agent"})
	server := releaseServer(t, "v2.0.0", archive, archiveChecksum(archive))
	defer server.Close()
	installDir := t.TempDir()
	beamPath := filepath.Join(installDir, version.CLIName())
	agentPath := filepath.Join(installDir, version.AgentName())
	writeExecutable(t, beamPath, "old beam")
	writeExecutable(t, agentPath, "old agent")
	var restarted atomic.Bool

	result, err := Run(context.Background(), Options{
		BaseURL:        server.URL,
		CurrentVersion: "v1.0.0",
		ExecutablePath: beamPath,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		AfterActivate: func(context.Context) error {
			assertFile(t, beamPath, "new beam")
			assertFile(t, agentPath, "new agent")
			restarted.Store(true)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.Deferred || !restarted.Load() {
		t.Fatalf("result=%#v restarted=%t", result, restarted.Load())
	}
	assertFile(t, beamPath, "new beam")
	assertFile(t, agentPath, "new agent")
}
