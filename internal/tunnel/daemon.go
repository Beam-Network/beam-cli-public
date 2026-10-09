package tunnel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func StartDaemon(ctx context.Context, options StartOptions) (StartResult, error) {
	client := NewLocalClient(options.SocketPath).WithCLIVersion(options.CLIVersion)
	status, statusErr := client.Status(ctx)
	if statusErr == nil {
		if !Compatible(status) {
			return StartResult{}, &Error{Kind: ErrVersion, Detail: VersionError(status).Error()}
		}
		return StartResult{Status: status, AlreadyRunning: true}, nil
	}
	_, bundleMismatch := incompatibleBundleVersion(statusErr)
	if !isUnavailable(statusErr) && !bundleMismatch {
		return StartResult{}, statusErr
	}

	binary, legacy, err := findAgentBinary(options.AgentBinary)
	if err != nil {
		return StartResult{}, err
	}
	if !isUnavailable(statusErr) {
		replaced, replaceErr := stopIncompatibleBundle(ctx, options.SocketPath, statusErr)
		if !replaced {
			return StartResult{}, statusErr
		}
		if replaceErr != nil {
			return StartResult{}, replaceErr
		}
	}
	logPath, err := daemonLogPath(options.LogPath)
	if err != nil {
		return StartResult{}, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return StartResult{}, fmt.Errorf("open %s log: %w", version.AgentName(), err)
	}
	defer logFile.Close()

	args := []string{"-socket", options.SocketPath}
	if legacy {
		args = append([]string{"daemon"}, args...)
	}
	if value := strings.TrimSpace(options.CoordinatorURL); value != "" {
		args = append(args, "-coordinator", value)
	}
	if value := strings.TrimSpace(options.CoordinatorURLs); value != "" {
		args = append(args, "-coordinators", value)
	}
	if options.DiscoverCoordinators {
		args = append(args, "-discover-coordinators")
	}
	if value := strings.TrimSpace(options.CoordinatorRegion); value != "" {
		args = append(args, "-coordinator-region", value)
	}
	if value := strings.TrimSpace(options.RelayID); value != "" {
		args = append(args, "-relay-id", value)
	}
	if value := strings.TrimSpace(options.Transport); value != "" {
		args = append(args, "-transport", value)
	}
	if value := strings.TrimSpace(options.AgentID); value != "" {
		args = append(args, "-agent-id", value)
	}
	command := exec.Command(binary, args...)
	command.Env = daemonEnvironment(os.Environ())
	command.Stdout = logFile
	command.Stderr = logFile
	prepareDetachedCommand(command)
	if err := command.Start(); err != nil {
		return StartResult{}, fmt.Errorf("start %s: %w", version.AgentName(), err)
	}

	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = command.Process.Kill()
			return StartResult{}, ctx.Err()
		case err := <-exited:
			if err == nil {
				err = fmt.Errorf("%s exited before becoming ready", version.AgentName())
			}
			return StartResult{}, fmt.Errorf("%s failed to start; inspect %s: %w", version.AgentName(), logPath, err)
		case <-timeout.C:
			_ = command.Process.Kill()
			return StartResult{}, fmt.Errorf("%s did not become ready; inspect %s", version.AgentName(), logPath)
		case <-ticker.C:
			status, err := client.Status(ctx)
			if err != nil {
				continue
			}
			if !Compatible(status) {
				_ = command.Process.Kill()
				return StartResult{}, &Error{Kind: ErrVersion, Detail: VersionError(status).Error()}
			}
			return StartResult{Status: status, PID: command.Process.Pid, LogPath: logPath}, nil
		}
	}
}

// cliOnlySecretEnvironment names environment variables that carry credentials
// only the CLI uses. The daemon never reads them, and it outlives the command
// that starts it, so passing them on would leave the secret readable from the
// daemon's environment (for example /proc/<pid>/environ) for its lifetime.
var cliOnlySecretEnvironment = []string{"BEAM_API_KEY"}

// daemonEnvironment returns environ without cliOnlySecretEnvironment. Names are
// matched case-insensitively because Windows environment names are.
func daemonEnvironment(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		secret := false
		for _, excluded := range cliOnlySecretEnvironment {
			if strings.EqualFold(name, excluded) {
				secret = true
				break
			}
		}
		if !secret {
			result = append(result, entry)
		}
	}
	return result
}

func stopIncompatibleBundle(ctx context.Context, socketPath string, statusErr error) (bool, error) {
	daemonVersion, ok := incompatibleBundleVersion(statusErr)
	if !ok {
		return false, nil
	}

	// The daemon accepts shutdown only from the bundle version it is already
	// running. Authenticate this one recovery request as that reported version,
	// then let the caller launch the newly installed companion binary.
	client := NewLocalClient(socketPath).WithCLIVersion(daemonVersion)
	if _, err := client.Shutdown(ctx); err != nil {
		if isUnavailable(err) {
			return true, WaitForDaemonOwnershipRelease(ctx, client.SocketPath)
		}
		return true, fmt.Errorf("stop incompatible %s bundle: %w", version.AgentName(), err)
	}
	if err := waitForDaemonShutdown(ctx, client); err != nil {
		return true, err
	}
	return true, nil
}

func incompatibleBundleVersion(err error) (string, bool) {
	var tunnelErr *Error
	if !errors.As(err, &tunnelErr) || tunnelErr.Kind != ErrVersion || tunnelErr.Code != "bundle_version_incompatible" {
		return "", false
	}
	daemonVersion, ok := tunnelErr.Details["daemon_version"].(string)
	daemonVersion = strings.TrimSpace(daemonVersion)
	return daemonVersion, ok && daemonVersion != ""
}

func waitForDaemonShutdown(ctx context.Context, client *LocalClient) error {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("incompatible %s did not finish stopping: %w", version.AgentName(), ctx.Err())
		case <-ticker.C:
			if _, err := client.Status(ctx); isUnavailable(err) {
				return WaitForDaemonOwnershipRelease(ctx, client.SocketPath)
			}
		}
	}
}

func findAgentBinary(configured string) (string, bool, error) {
	if value := strings.TrimSpace(configured); value != "" {
		path, err := executableFile(value)
		return path, strings.Contains(strings.ToLower(filepath.Base(path)), "beam-agent") && !strings.Contains(strings.ToLower(filepath.Base(path)), "beam-agentd"), err
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_TUNNEL_AGENT_BINARY")); value != "" {
		path, err := executableFile(value)
		return path, false, err
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_AGENTD_BINARY")); value != "" {
		path, err := executableFile(value)
		return path, false, err
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_AGENT_BINARY")); value != "" {
		path, err := executableFile(value)
		return path, true, err
	}
	if current, err := os.Executable(); err == nil {
		name := version.ExecutableName(version.AgentName(), runtime.GOOS)
		candidate := filepath.Join(filepath.Dir(current), name)
		if path, err := executableFile(candidate); err == nil {
			return path, false, nil
		}
	}
	path, err := exec.LookPath(version.AgentName())
	if err == nil {
		return path, false, nil
	}
	// One release window of discovery compatibility for installations which
	// still contain the old binary. Protocol negotiation prevents silently
	// running an incompatible legacy daemon.
	oldDaemonPath, oldDaemonErr := exec.LookPath("beam-agentd")
	if oldDaemonErr == nil {
		return oldDaemonPath, false, nil
	}
	legacyPath, legacyErr := exec.LookPath("beam-agent")
	if legacyErr == nil {
		return legacyPath, true, nil
	}
	return "", false, fmt.Errorf("%s companion binary was not found; install the Beam bundle or set BEAM_TUNNEL_AGENT_BINARY", version.AgentName())
}

func executableFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect %s binary: %w", version.AgentName(), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s binary must be a regular file", version.AgentName())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s binary is not executable", version.AgentName())
	}
	return path, nil
}

func daemonLogPath(configured string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		path = filepath.Join(home, "."+version.StateNamespace(), "agent.log")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create %s log directory: %w", version.AgentName(), err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", fmt.Errorf("secure %s log directory: %w", version.AgentName(), err)
	}
	return path, nil
}

func isUnavailable(err error) bool {
	var tunnelErr *Error
	return errors.As(err, &tunnelErr) && tunnelErr.Kind == ErrUnavailable
}
