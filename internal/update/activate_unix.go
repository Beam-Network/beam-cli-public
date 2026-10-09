//go:build !windows

package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func activate(sourceDir, executablePath, _ string) (deferred, preserveSource bool, err error) {
	installDir := filepath.Dir(executablePath)
	agentName := version.AgentName()
	agentPath := filepath.Join(installDir, agentName)
	newBeam, err := stageInInstallDir(filepath.Join(sourceDir, version.CLIName()), installDir, ".beam-update-")
	if err != nil {
		return false, false, writableInstallError(installDir, err)
	}
	defer os.Remove(newBeam)
	newAgent, err := stageInInstallDir(filepath.Join(sourceDir, agentName), installDir, ".beam-tunnel-agent-update-")
	if err != nil {
		return false, false, writableInstallError(installDir, err)
	}
	defer os.Remove(newAgent)

	agentBackup := ""
	if _, statErr := os.Stat(agentPath); statErr == nil {
		agentBackup, err = stageInInstallDir(agentPath, installDir, ".beam-tunnel-agent-backup-")
		if err != nil {
			return false, false, writableInstallError(installDir, err)
		}
		defer os.Remove(agentBackup)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, false, statErr
	}

	if err := os.Rename(newAgent, agentPath); err != nil {
		return false, false, writableInstallError(installDir, err)
	}
	if err := os.Rename(newBeam, executablePath); err != nil {
		if agentBackup != "" {
			_ = os.Rename(agentBackup, agentPath)
		} else {
			_ = os.Remove(agentPath)
		}
		return false, false, writableInstallError(installDir, err)
	}
	if directory, openErr := os.Open(installDir); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return false, false, nil
}

func stageInInstallDir(sourcePath, installDir, pattern string) (string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	target, err := os.CreateTemp(installDir, pattern)
	if err != nil {
		return "", err
	}
	targetPath := target.Name()
	remove := true
	defer func() {
		_ = target.Close()
		if remove {
			_ = os.Remove(targetPath)
		}
	}()
	if err := target.Chmod(0o755); err != nil {
		return "", err
	}
	if _, err := io.Copy(target, source); err != nil {
		return "", err
	}
	if err := target.Sync(); err != nil {
		return "", err
	}
	if err := target.Close(); err != nil {
		return "", err
	}
	remove = false
	return targetPath, nil
}

func writableInstallError(installDir string, err error) error {
	return fmt.Errorf("cannot replace binaries in %s: %w", installDir, err)
}
