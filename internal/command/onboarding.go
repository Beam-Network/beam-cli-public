package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

const (
	setupModeAccount = "account"
	setupModeRoom    = "room"
	setupModeSkip    = "skip"
)

var setupValueFlags = map[string]bool{
	"--mode": true, "--coordinator": true, "--organization": true, "--label": true,
	"--enrollment-token": true, "--room": true, "--invitation-file": true,
	"--invitation-token": true, "--completion": true,
}

func (a *App) setup(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if err := validateOptions(args, setupValueFlags, nil); err != nil {
		return err
	}
	mode, _, err := flag(args, "--mode")
	if err != nil {
		return err
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "" && mode != setupModeAccount && mode != setupModeRoom && mode != setupModeSkip {
		return usage("--mode must be account, room, or skip")
	}
	if err := validateSetupModeOptions(args, mode); err != nil {
		return err
	}
	completion, completionProvided, err := flag(args, "--completion")
	if err != nil {
		return err
	}
	if completionProvided {
		completion = strings.ToLower(strings.TrimSpace(completion))
		if completion != "none" {
			if _, supported := completionScript(completion); !supported {
				return usage("--completion must be bash, zsh, fish, powershell, or none")
			}
		}
	}

	if !renderer.Interactive {
		if mode == "" {
			return typedCLIError(ExitUsage, errorKindSetupModeRequired, "Non-interactive setup requires --mode.", "Use --mode account, --mode room, or --mode skip.", nil)
		}
		var setupErr error
		if mode == setupModeRoom {
			setupErr = a.setupRoom(ctx, args, cfg, paths, renderer, false)
		} else if mode == setupModeSkip {
			if completionProvided {
				a.setupCompletion(args, renderer)
			}
			return renderSetupSkipped(renderer)
		} else {
			setupErr = a.agentConnect(ctx, agentSetupArgs(args), cfg, paths, tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version), renderer)
		}
		if setupErr != nil {
			return setupErr
		}
		if completionProvided {
			a.setupCompletion(args, renderer)
		}
		return nil
	}

	renderer.Progress("")
	renderer.Progress("Welcome to Beam")
	renderer.Progress("Let's configure this machine.")
	renderer.Progress("")
	if mode == "" {
		mode, err = a.promptSetupMode(renderer)
		if err != nil {
			return err
		}
	}

	switch mode {
	case setupModeSkip:
		a.setupCompletion(args, renderer)
		return renderSetupSkipped(renderer)
	case setupModeRoom:
		if err := a.setupRoom(ctx, args, cfg, paths, renderer, true); err != nil {
			return err
		}
	case setupModeAccount:
		nextMode, err := a.setupAccount(ctx, args, cfg, paths, renderer)
		if err != nil {
			return err
		}
		if nextMode == setupModeRoom {
			if err := a.setupRoom(ctx, args, cfg, paths, renderer, true); err != nil {
				return err
			}
		} else if nextMode == setupModeSkip {
			a.setupCompletion(args, renderer)
			return renderSetupSkipped(renderer)
		}
	}

	a.setupCompletion(args, renderer)
	renderer.Progress("")
	renderer.Progress("✓ Beam onboarding complete.")
	renderer.Progress("  Try: " + applyReleaseNames("beam status"))
	return nil
}

func validateSetupModeOptions(args []string, mode string) error {
	var disallowed []string
	switch mode {
	case setupModeAccount:
		disallowed = []string{"--room", "--invitation-file", "--invitation-token"}
	case setupModeRoom:
		disallowed = []string{"--organization", "--label", "--enrollment-token"}
	case setupModeSkip:
		disallowed = []string{"--coordinator", "--organization", "--label", "--enrollment-token", "--room", "--invitation-file", "--invitation-token"}
	default:
		return nil
	}
	for _, name := range disallowed {
		if hasFlag(args, name) {
			return usage(name + " is not valid with --mode " + mode)
		}
	}
	return nil
}

func (a *App) setupAccount(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) (string, error) {
	connectArgs := agentSetupArgs(args)
	client := tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version)
	consoleOpened := false
	for {
		err := a.agentConnect(ctx, connectArgs, cfg, paths, client, renderer)
		if err == nil {
			return "", nil
		}
		cliErr := asCLIError(err)
		if cliErr.Kind != errorKindOrganizationRequired && cliErr.Kind != errorKindOrganizationNotFound && cliErr.Kind != errorKindOrganizationUnavailable && cliErr.Kind != errorKindAccountUnavailable {
			return "", err
		}
		renderer.Progress("")
		renderer.Progress(cliErr.Message)
		if !consoleOpened {
			if consoleURL := organizationOnboardingURL(cfg.APIURL); consoleURL != "" {
				if a.openURL != nil {
					_ = a.openURL(consoleURL)
				}
				renderer.Progress("Beam Console: " + renderer.Hyperlink(consoleURL))
				consoleOpened = true
			}
		}
		if cliErr.Kind == errorKindAccountUnavailable {
			renderer.Progress("Resolve the account restriction in the Beam Console, then refresh here.")
		} else if cliErr.Kind == errorKindOrganizationUnavailable {
			renderer.Progress("The organizations attached to this account are currently restricted.")
		} else if cliErr.Kind == errorKindOrganizationNotFound {
			renderer.Progress("Refresh to select from the organizations that are still accessible.")
		} else {
			renderer.Progress("Create or join an organization in the Beam Console, then refresh here.")
		}
		renderer.Progress("")
		renderer.Progress("  1. Refresh organizations")
		renderer.Progress("  2. Join a Room with an invitation")
		renderer.Progress("  3. Finish configuration later")
		for {
			choice, promptErr := a.promptText(renderer, "Choice", "1", true)
			if promptErr != nil {
				return "", promptErr
			}
			switch strings.ToLower(choice) {
			case "1", "refresh", "retry":
				break
			case "2", "room", "join":
				return setupModeRoom, nil
			case "3", "skip", "later":
				return setupModeSkip, nil
			default:
				renderer.Progress("Please enter 1, 2, or 3.")
				continue
			}
			break
		}
	}
}

func organizationOnboardingURL(apiURL string) string {
	if configured := strings.TrimSpace(os.Getenv("BEAM_CONSOLE_URL")); configured != "" {
		parsed, err := url.Parse(configured)
		if err == nil && (parsed.Scheme == "https" || (parsed.Scheme == "http" && parsed.Hostname() == "localhost")) && parsed.Host != "" {
			if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/onboarding") {
				parsed.Path = strings.TrimRight(parsed.Path, "/") + "/onboarding"
			}
			return parsed.String()
		}
		return ""
	}
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return ""
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "api.b1m.ai":
		return "https://console.b1m.ai/onboarding"
	default:
		return ""
	}
}

func (a *App) promptSetupMode(renderer output.Renderer) (string, error) {
	renderer.Progress("What would you like to do?")
	renderer.Progress("  1. Connect this machine to my Beam account")
	renderer.Progress("  2. Join a Room with an invitation")
	renderer.Progress("  3. Finish installation without configuration")
	for {
		choice, err := a.promptText(renderer, "Choice", "1", true)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(choice) {
		case "1", "account", "connect":
			return setupModeAccount, nil
		case "2", "room", "join":
			return setupModeRoom, nil
		case "3", "skip", "later":
			return setupModeSkip, nil
		default:
			renderer.Progress("Please enter 1, 2, or 3.")
		}
	}
}

func (a *App) setupRoom(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer, interactive bool) error {
	roomID, _, err := flag(args, "--room")
	if err != nil {
		return err
	}
	invitationFile, _, err := flag(args, "--invitation-file")
	if err != nil {
		return err
	}
	invitationToken, _, err := flag(args, "--invitation-token")
	if err != nil {
		return err
	}
	coordinator, _, err := flag(args, "--coordinator")
	if err != nil {
		return err
	}
	if invitationFile != "" && invitationToken != "" {
		return usage("setup accepts only one of --invitation-file or --invitation-token")
	}
	if interactive && strings.TrimSpace(roomID) == "" {
		roomID, err = a.promptText(renderer, "Room ID", "", true)
		if err != nil {
			return err
		}
	}
	if interactive && invitationFile == "" && invitationToken == "" {
		invitationFile, err = a.promptText(renderer, "Invitation file", "invitation.token", true)
		if err != nil {
			return err
		}
	}
	roomArgs := []string{roomID}
	if invitationFile != "" {
		roomArgs = append(roomArgs, "--invitation-file", invitationFile)
	}
	if invitationToken != "" {
		roomArgs = append(roomArgs, "--invitation-token", invitationToken)
	}
	if coordinator != "" {
		roomArgs = append(roomArgs, "--coordinator", coordinator)
	}
	return a.roomJoin(ctx, roomArgs, cfg, paths, renderer)
}

func agentSetupArgs(args []string) []string {
	var result []string
	for _, name := range []string{"--coordinator", "--organization", "--label", "--enrollment-token"} {
		if value, found, err := flag(args, name); err == nil && found {
			result = append(result, name, value)
		}
	}
	return result
}

func (a *App) setupCompletion(args []string, renderer output.Renderer) {
	requested, provided, err := flag(args, "--completion")
	if err != nil {
		return
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "none" || requested == "off" {
		return
	}
	if !provided {
		requested = detectedCompletionShell()
		if _, supported := completionScript(requested); !supported {
			return
		}
		enabled, promptErr := a.promptYesNo(renderer, "Enable "+requested+" completion", true)
		if promptErr != nil || !enabled {
			return
		}
	}
	path, shell, installErr := installCompletion(requested)
	if installErr != nil {
		renderer.Progress("Warning: completion could not be installed: " + installErr.Error())
		return
	}
	renderer.Progress(fmt.Sprintf("✓ Installed %s completion at %s.", shell, path))
}

func detectedCompletionShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return filepath.Base(os.Getenv("SHELL"))
}

func (a *App) promptYesNo(renderer output.Renderer, label string, defaultYes bool) (bool, error) {
	fallback := "y/N"
	if defaultYes {
		fallback = "Y/n"
	}
	for {
		value, err := a.promptText(renderer, label+" ["+fallback+"]", "", false)
		if err != nil {
			return false, err
		}
		if value == "" {
			return defaultYes, nil
		}
		switch strings.ToLower(value) {
		case "y", "yes", "o", "oui":
			return true, nil
		case "n", "no", "non":
			return false, nil
		default:
			renderer.Progress("Please answer yes or no.")
		}
	}
}

func (a *App) promptText(renderer output.Renderer, label, fallback string, required bool) (string, error) {
	for {
		display := label
		if fallback != "" {
			display += " [" + fallback + "]"
		}
		if _, err := fmt.Fprint(renderer.Err, display+": "); err != nil {
			return "", err
		}
		if a.readLine == nil {
			return "", cliError(ExitInterrupted, "Interactive setup input is unavailable.", "Run setup again in a terminal.", nil)
		}
		line, err := a.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return "", cliError(ExitInterrupted, "Could not read interactive setup input.", "Run setup again.", err)
		}
		if errors.Is(err, io.EOF) && line == "" {
			return "", cliError(ExitInterrupted, "Interactive setup was cancelled.", "Run setup again when you are ready.", nil)
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = strings.TrimSpace(fallback)
		}
		if required && value == "" {
			renderer.Progress(label + " is required.")
			continue
		}
		return value, nil
	}
}

func (a *App) promptOrganization(organizations []auth.Organization, renderer output.Renderer) (auth.Organization, error) {
	renderer.Progress("Choose a Beam organization:")
	selectable := availableOrganizations(organizations)
	for index, organization := range selectable {
		label := firstNonEmpty(organization.Name, organization.Slug, organization.PublicID, organization.ID)
		detail := firstNonEmpty(organization.Slug, organization.PublicID, organization.ID)
		renderer.Progress(fmt.Sprintf("  %d. %s (%s)", index+1, label, detail))
	}
	for _, organization := range organizations {
		if organizationAvailable(organization) {
			continue
		}
		label := firstNonEmpty(organization.Name, organization.Slug, organization.PublicID, organization.ID)
		renderer.Progress(fmt.Sprintf("  - %s (unavailable: %s)", label, normalizedRestriction(organization)))
	}
	if len(selectable) == 0 {
		return auth.Organization{}, organizationUnavailableError(organizations)
	}
	for {
		choice, err := a.promptText(renderer, "Organization", "", true)
		if err != nil {
			return auth.Organization{}, err
		}
		if index, parseErr := strconv.Atoi(choice); parseErr == nil && index >= 1 && index <= len(selectable) {
			return selectable[index-1], nil
		}
		if organization, found := findOrganization(selectable, choice); found {
			return organization, nil
		}
		renderer.Progress("Choose one of the listed organizations by number, ID, or slug.")
	}
}

func renderSetupSkipped(renderer output.Renderer) error {
	return renderer.Result(map[string]any{"configured": false, "skipped": true}, func(w io.Writer) error {
		_, err := fmt.Fprintln(w, applyReleaseNames("Beam is installed. Run `beam setup` when you are ready."))
		return err
	})
}
