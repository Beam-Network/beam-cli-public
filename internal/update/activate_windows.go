//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const windowsUpdateScript = `param(
  [string]$SourceDir,
  [string]$InstallDir,
  [string]$CLIName,
  [string]$AgentName
)
$ErrorActionPreference = "Stop"
$beamSource = Join-Path $SourceDir $CLIName
$agentSource = Join-Path $SourceDir $AgentName
$beamTarget = Join-Path $InstallDir $CLIName
$agentTarget = Join-Path $InstallDir $AgentName
$beamNew = Join-Path $InstallDir ".$CLIName.update"
$agentNew = Join-Path $InstallDir ".$AgentName.update"
$agentBackup = Join-Path $InstallDir ".$AgentName.backup"
$beamBackup = Join-Path $InstallDir ".$CLIName.backup"
$errorLog = Join-Path $InstallDir ".beam-update-error.log"
$agentExisted = Test-Path $agentTarget
$deadline = [DateTime]::UtcNow.AddSeconds(30)
$lastError = $null
while ([DateTime]::UtcNow -lt $deadline) {
  try {
    Remove-Item $beamNew,$agentNew,$agentBackup,$beamBackup -Force -ErrorAction SilentlyContinue
    Copy-Item $beamSource $beamNew -Force
    Copy-Item $agentSource $agentNew -Force
    if ($agentExisted) {
      [IO.File]::Replace($agentNew, $agentTarget, $agentBackup, $true)
    } else {
      [IO.File]::Move($agentNew, $agentTarget)
    }
    try {
      if (Test-Path $beamTarget) {
        [IO.File]::Replace($beamNew, $beamTarget, $beamBackup, $true)
      } else {
        [IO.File]::Move($beamNew, $beamTarget)
      }
    } catch {
      if ($agentExisted -and (Test-Path $agentBackup)) {
        [IO.File]::Replace($agentBackup, $agentTarget, $null, $true)
      } elseif (-not $agentExisted) {
        Remove-Item $agentTarget -Force -ErrorAction SilentlyContinue
      }
      throw
    }
    Remove-Item $agentBackup,$beamBackup,$errorLog -Force -ErrorAction SilentlyContinue
    Remove-Item $SourceDir -Recurse -Force -ErrorAction SilentlyContinue
    exit 0
  } catch {
    $lastError = $_
    Remove-Item $beamNew,$agentNew,$agentBackup,$beamBackup -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 100
  }
}
($lastError | Out-String) | Set-Content -Path $errorLog
Remove-Item $beamNew,$agentNew,$agentBackup,$beamBackup -Force -ErrorAction SilentlyContinue
Remove-Item $SourceDir -Recurse -Force -ErrorAction SilentlyContinue
`

const (
	windowsCreateBreakawayFromJob = 0x01000000
	windowsCreateNoWindow         = 0x08000000
)

func activate(sourceDir, executablePath, _ string) (deferred, preserveSource bool, err error) {
	scriptPath := filepath.Join(sourceDir, "finish-update.ps1")
	if err := os.WriteFile(scriptPath, []byte(windowsUpdateScript), 0o600); err != nil {
		return false, false, err
	}
	command := exec.Command(
		"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", scriptPath,
		"-SourceDir", sourceDir,
		"-InstallDir", filepath.Dir(executablePath),
		"-CLIName", filepath.Base(executablePath),
		"-AgentName", version.ExecutableName(version.AgentName(), "windows"),
	)
	command.SysProcAttr = &syscall.SysProcAttr{
		// A CLI can itself run in a kill-on-close job (terminals, package
		// managers and automation commonly do this). Without breakaway, that
		// job can terminate the deferred updater as soon as beam exits, before
		// either verified binary is replaced.
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windowsCreateNoWindow | windowsCreateBreakawayFromJob,
		HideWindow:    true,
	}
	if err := command.Start(); err != nil {
		// Windows rejects CREATE_BREAKAWAY_FROM_JOB when the containing job
		// does not permit breakaway. In that environment the best available
		// option is to keep the updater in the current job; retry only for the
		// specific access-denied response so other launch failures remain
		// visible.
		if !os.IsPermission(err) {
			return false, false, fmt.Errorf("schedule replacement after %s exits: %w", version.CLIName(), err)
		}
		command = exec.Command(
			"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", scriptPath,
			"-SourceDir", sourceDir,
			"-InstallDir", filepath.Dir(executablePath),
			"-CLIName", filepath.Base(executablePath),
			"-AgentName", version.ExecutableName(version.AgentName(), "windows"),
		)
		command.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windowsCreateNoWindow,
			HideWindow:    true,
		}
		if fallbackErr := command.Start(); fallbackErr != nil {
			return false, false, fmt.Errorf("schedule replacement after %s exits: %w", version.CLIName(), fallbackErr)
		}
	}
	_ = command.Process.Release()
	return true, true, nil
}
