package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	beamupdate "github.com/Beam-Network/beam-cli-public/internal/update"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func (a *App) update(ctx context.Context, args []string, cfg config.Config, renderer output.Renderer) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		if len(args) != 1 {
			return usage("update help does not accept arguments")
		}
		return renderNamedHelp("update", renderer)
	}
	if err := validateOptions(args,
		map[string]bool{"--version": true},
		map[string]bool{"--check": true, "--force": true},
	); err != nil {
		return err
	}
	positions, err := updatePositionals(args)
	if err != nil {
		return err
	}
	if len(positions) != 0 {
		return usage("update does not accept positional arguments")
	}
	checkOnly := has(args, "--check")
	force := has(args, "--force")
	if checkOnly && force {
		return usage("--check and --force cannot be used together")
	}
	target, _, err := flag(args, "--version")
	if err != nil {
		return err
	}

	spinner := renderer.StartSpinner("Checking for Beam updates")
	defer spinner.Stop()
	options := beamupdate.Options{
		BaseURL:         os.Getenv("BEAM_CDN_BASE_URL"),
		CurrentVersion:  a.version.Version,
		TargetVersion:   target,
		ChannelManifest: version.ChannelManifest(),
		CheckOnly:       checkOnly,
		Force:           force,
	}
	if checkOnly {
		// A read-only update check never needs daemon lifecycle hooks.
	} else if runtime.GOOS == "windows" {
		options.BeforeActivate = func(ctx context.Context) error {
			spinner.Update("Stopping " + version.AgentName())
			return stopDaemonForUpdate(ctx, cfg.AgentSocket)
		}
	} else {
		// Unix can atomically replace an executing binary. Replace both files
		// first, then ask the old daemon to exit so launchd/systemd restarts the
		// newly installed companion. Stopping first races keepalive supervisors,
		// which can restart the old daemon before replacement and make updates
		// time out while waiting for a socket that never stays absent.
		options.AfterActivate = func(ctx context.Context) error {
			spinner.Update("Restarting " + version.AgentName())
			return signalDaemonAfterUpdate(ctx, cfg.AgentSocket)
		}
	}
	result, err := a.updateRun(ctx, options)
	spinner.Stop()
	if err != nil {
		return cliError(
			ExitOperationFailed,
			"Beam could not be updated.",
			updateHint(err),
			err,
		)
	}
	return renderer.Result(result, func(w io.Writer) error {
		switch {
		case checkOnly && result.UpdateAvailable:
			_, err := fmt.Fprintf(w, "Beam update available: %s -> %s\n", displayVersion(result.CurrentVersion), result.Version)
			return err
		case checkOnly:
			_, err := fmt.Fprintf(w, "Beam %s is up to date\n", result.Version)
			return err
		case result.Deferred:
			_, err := fmt.Fprintf(w, "Beam %s update scheduled; installation will finish after this process exits\n", result.Version)
			return err
		case result.Updated:
			_, err := fmt.Fprintf(w, "Updated %s and %s to %s in %s\n", version.CLIName(), version.AgentName(), result.Version, result.InstallDir)
			return err
		default:
			_, err := fmt.Fprintf(w, "Beam %s is already up to date\n", result.Version)
			return err
		}
	})
}

func signalDaemonAfterUpdate(ctx context.Context, socketPath string) error {
	return stopDaemonForUpdate(ctx, socketPath)
}

func stopDaemonForUpdate(ctx context.Context, socketPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	client := tunnel.NewLocalClient(socketPath)
	_, err := client.Shutdown(ctx)
	if err != nil {
		if daemonUnavailable(err) {
			return tunnel.WaitForDaemonOwnershipRelease(ctx, socketPath)
		}
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent did not finish stopping for update: %w", ctx.Err())
		case <-ticker.C:
			_, statusErr := client.Status(ctx)
			if daemonUnavailable(statusErr) {
				return tunnel.WaitForDaemonOwnershipRelease(ctx, socketPath)
			}
		}
	}
}

func daemonUnavailable(err error) bool {
	var tunnelErr *tunnel.Error
	return errors.As(err, &tunnelErr) && tunnelErr.Kind == tunnel.ErrUnavailable
}

func updateHint(err error) string {
	message := strings.ToLower(err.Error())
	if errors.Is(err, syscall.ENOSPC) || strings.Contains(message, "no space left on device") ||
		strings.Contains(message, "not enough space on the disk") || strings.Contains(message, "disk full") {
		return "Free space on the install filesystem, then retry the update."
	}
	if strings.Contains(message, "cannot replace binaries") || strings.Contains(message, "permission denied") {
		return "The install directory is not writable. Re-run the installer, which can request elevation."
	}
	if strings.Contains(message, "checksum") {
		return "The downloaded bundle was not installed. Check the CDN release and try again."
	}
	if strings.Contains(message, "not published") {
		return "Install Beam on a supported macOS/Linux arm64 or amd64, or Windows amd64 system."
	}
	return "Check your network connection and BEAM_CDN_BASE_URL, then try again."
}

func updatePositionals(args []string) ([]string, error) {
	var result []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--check", "--force":
			continue
		case "--version":
			index++
			if index >= len(args) {
				return nil, usage("--version requires a value")
			}
		default:
			if strings.HasPrefix(args[index], "--version=") {
				continue
			}
			result = append(result, args[index])
		}
	}
	return result, nil
}

func displayVersion(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
