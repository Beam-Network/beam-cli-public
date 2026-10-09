package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func (a *App) overallStatus(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) != 0 {
		return usage("status does not accept arguments")
	}
	result := map[string]any{
		"context":       paths.ContextName,
		"organization":  cfg.Organization,
		"configuration": "valid",
	}
	credentials, credentialErr := a.authStore(paths.Credentials).Load()
	result["authenticated"] = credentialErr == nil && credentials.Active()
	if credentials.User != nil {
		result["user"] = credentials.User
	}
	if credentialErr != nil {
		result["credentials_error"] = credentialErr.Error()
	}

	client := tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version)
	daemonStatus, daemonErr := client.Status(ctx)
	result["daemon_available"] = daemonErr == nil
	if daemonErr == nil {
		result["daemon"] = daemonStatus
		result["daemon_compatible"] = tunnel.Compatible(daemonStatus)
		if agentStatus, err := client.AgentConnection(ctx); err == nil {
			result["agent"] = agentStatus
		}
		if metrics, err := client.Metrics(ctx); err == nil {
			result["metrics"] = metrics
		}
	} else {
		result["daemon_error"] = daemonErr.Error()
	}

	return renderer.Result(result, func(w io.Writer) error {
		// Session state decides, identity only labels it: the stored user
		// outlives the refresh token, so testing it first reports an expired
		// session as logged in.
		account := "not logged in"
		if credentials.Active() {
			account = "logged in"
			if credentials.User != nil {
				account = firstNonEmpty(credentials.User.Email, credentials.User.Name, credentials.User.ID)
			}
		}
		contextName := firstNonEmpty(paths.ContextName, "default")
		_, _ = fmt.Fprintf(w, "Context: %s\nAccount: %s\n", contextName, account)
		if daemonErr != nil {
			_, _ = fmt.Fprintln(w, "Daemon: stopped")
			_, _ = fmt.Fprintln(w, applyReleaseNames("Next: beam setup"))
			return nil
		}
		compatibility := "compatible"
		if !tunnel.Compatible(daemonStatus) {
			compatibility = "incompatible"
		}
		_, _ = fmt.Fprintf(w, "Daemon: ready=%t, %s, local API v%d\n", daemonStatus.Ready, compatibility, daemonStatus.ProtocolVersion)
		if agent, ok := result["agent"].(tunnel.AgentConnectionStatus); ok {
			_, _ = fmt.Fprintf(w, "Agent: %s, %s\n", firstNonEmpty(agent.Label, agent.AgentID, "unregistered"), firstNonEmpty(agent.State, "unknown"))
		}
		if metrics, ok := result["metrics"].(tunnel.Metrics); ok && tunnelsEnabled() {
			_, _ = fmt.Fprintf(w, "Tunnels: %d active / %d total\nOperations: %d running / %d total\n", metrics.EndpointsActive, metrics.EndpointsTotal, metrics.OperationsRunning, metrics.OperationsTotal)
		}
		return nil
	})
}

func (a *App) doctor(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if err := validateOptions(args, nil, map[string]bool{"--fix": true}); err != nil {
		return err
	}
	fix := has(args, "--fix")
	checks := []map[string]any{{"name": "configuration", "ok": true, "detail": "configuration is valid"}}

	credentials, err := a.authStore(paths.Credentials).Load()
	checks = append(checks, map[string]any{"name": "authentication", "ok": err == nil && credentials.Active(), "detail": doctorDetail(err, credentials.Active(), "Beam login is active", "run `beam login`")})

	client := tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version)
	status, daemonErr := client.Status(ctx)
	if daemonErr != nil && fix {
		_, startErr := a.startAgentDaemon(ctx, cfg, cfg.CoordinatorURL)
		if startErr == nil {
			status, daemonErr = client.Status(ctx)
		}
	}
	checks = append(checks, map[string]any{"name": "daemon", "ok": daemonErr == nil, "detail": doctorDetail(daemonErr, daemonErr == nil, "beam-agentd is running", "run `beam doctor --fix`")})
	compatible := daemonErr == nil && tunnel.Compatible(status)
	checks = append(checks, map[string]any{"name": "versions", "ok": compatible, "detail": doctorDetail(nil, compatible, "CLI and daemon are compatible", "run `beam update`")})

	agentOK := false
	agentDetail := "connect this machine with `beam setup`"
	if daemonErr == nil {
		if agent, agentErr := client.AgentConnection(ctx); agentErr == nil {
			agentOK = agent.Registered && agent.Connected
			agentDetail = firstNonEmpty(agent.LastError, agent.State, agentDetail)
			// Invitation-only onboarding uses the room machine identity.
			if err == nil && !credentials.Active() && agent.RoomJoinOnly && agentOK {
				checks[1]["ok"] = true
				checks[1]["detail"] = "Room invitation identity is active"
			}
		} else {
			agentDetail = agentErr.Error()
		}
	}
	checks = append(checks, map[string]any{"name": "agent", "ok": agentOK, "detail": agentDetail})

	healthy := true
	for _, check := range checks {
		if ok, _ := check["ok"].(bool); !ok {
			healthy = false
		}
	}
	result := map[string]any{"healthy": healthy, "fixed": fix, "context": paths.ContextName, "checks": checks}
	return renderer.Result(result, func(w io.Writer) error {
		for _, check := range checks {
			mark := "✓"
			if ok, _ := check["ok"].(bool); !ok {
				mark = "✗"
			}
			_, _ = fmt.Fprintf(w, "%s %-16s %s\n", mark, check["name"], check["detail"])
		}
		if !healthy {
			_, _ = fmt.Fprintln(w, applyReleaseNames("\nRun `beam doctor --fix` for safe automatic repairs or `beam setup` to finish onboarding."))
		}
		return nil
	})
}

func doctorDetail(err error, ok bool, success, fallback string) string {
	if err != nil {
		return applyReleaseNames(strings.ReplaceAll(err.Error(), "beam-agent is", "beam-agentd is"))
	}
	if ok {
		return applyReleaseNames(success)
	}
	return applyReleaseNames(fallback)
}

func (a *App) sessionCommand(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return usage("session status does not accept arguments")
		}
		credentials, err := a.authStore(paths.Credentials).Load()
		if err != nil {
			return cliError(ExitConfig, "Could not read Beam credentials.", "Run `beam doctor`.", err)
		}
		result := map[string]any{"authenticated": credentials.Active(), "user": credentials.User, "organizations": credentials.Organizations}
		return renderer.Result(result, func(w io.Writer) error {
			if !credentials.Active() {
				_, err := fmt.Fprintln(w, applyReleaseNames("No active Beam session. Run `beam login`."))
				return err
			}
			_, err := fmt.Fprintf(w, "Active Beam session for %s.\n", identityLabel(credentials.User))
			return err
		})
	case "refresh":
		if len(args) != 1 {
			return usage("session refresh does not accept arguments")
		}
		store := a.authStore(paths.Credentials)
		session := &auth.Session{Client: auth.DeviceClient{BaseURL: cfg.AuthURL}, Store: store}
		return authWhoami(ctx, cfg, session, renderer)
	default:
		return usage(fmt.Sprintf("unknown session command %q", args[0]))
	}
}

func (a *App) organizationCommand(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"current"}
	}
	credentials, err := a.authStore(paths.Credentials).Load()
	if err != nil {
		return cliError(ExitConfig, "Could not read Beam organizations.", "Run `beam login` again.", err)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage("org list does not accept arguments")
		}
		return renderer.Result(map[string]any{"current": currentOrganization(cfg, credentials.Organizations), "organizations": credentials.Organizations}, func(w io.Writer) error {
			current := currentOrganization(cfg, credentials.Organizations)
			rows := make([][]string, 0, len(credentials.Organizations))
			for _, organization := range credentials.Organizations {
				mark := " "
				if organization.ID == current.ID {
					mark = "*"
				}
				rows = append(rows, []string{mark, firstNonEmpty(organization.Slug, organization.PublicID, organization.ID), organization.Name, organization.Role})
			}
			return renderer.Table(w, output.Table{
				// The marker column carries no header: a single "*" against the
				// selected organization explains itself, and a word above it would
				// be wider than the column it labels.
				Headers: []string{"", "ORGANIZATION", "NAME", "ROLE"},
				Rows:    rows,
				Style: func(column int, value string) output.Style {
					if column == 0 && value == "*" {
						return output.StyleOK
					}
					if column == 1 {
						return output.StyleID
					}
					return output.StyleNone
				},
			})
		})
	case "current":
		if len(args) != 1 {
			return usage("org current does not accept arguments")
		}
		organization := currentOrganization(cfg, credentials.Organizations)
		if organization.ID == "" {
			return cliError(ExitNotFound, "No Beam organization is selected.", "Run `beam org list` then `beam org use <slug>`.", nil)
		}
		return renderOrganization(organization, renderer)
	case "use":
		if len(args) != 2 {
			return usage("org use requires an organization ID or slug")
		}
		organization, found := findOrganization(credentials.Organizations, args[1])
		if !found {
			return cliError(ExitNotFound, "Beam organization was not found.", "Run `beam org list`.", nil)
		}
		if !organizationAvailable(organization) {
			return organizationUnavailableError([]auth.Organization{organization})
		}
		cfg.Organization = organization.ID
		if err := config.Save(paths, cfg); err != nil {
			return cliError(ExitConfig, "Could not save the active organization.", "Check the Beam config directory permissions.", err)
		}
		return renderOrganization(organization, renderer)
	case "show":
		if len(args) != 2 {
			return usage("org show requires an organization ID or slug")
		}
		organization, found := findOrganization(credentials.Organizations, args[1])
		if !found {
			return cliError(ExitNotFound, "Beam organization was not found.", "Run `beam org list`.", nil)
		}
		return renderOrganization(organization, renderer)
	default:
		return usage(fmt.Sprintf("unknown org command %q", args[0]))
	}
}

func renderOrganization(organization auth.Organization, renderer output.Renderer) error {
	return renderer.Result(organization, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", organization.ID, firstNonEmpty(organization.Slug, organization.PublicID), organization.Name, organization.Role)
		return err
	})
}

func currentOrganization(cfg config.Config, organizations []auth.Organization) auth.Organization {
	if cfg.Organization != "" {
		if organization, found := findOrganization(organizations, cfg.Organization); found && organizationAvailable(organization) {
			return organization
		}
	}
	for _, organization := range organizations {
		if organization.IsDefault && organizationAvailable(organization) {
			return organization
		}
	}
	available := availableOrganizations(organizations)
	if len(available) == 1 {
		return available[0]
	}
	return auth.Organization{}
}

func findOrganization(organizations []auth.Organization, requested string) (auth.Organization, bool) {
	for _, organization := range organizations {
		if requested == organization.ID || requested == organization.PublicID || requested == organization.Slug {
			return organization, true
		}
	}
	return auth.Organization{}, false
}

func (a *App) contextCommand(args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"current"}
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage("context list does not accept arguments")
		}
		contexts, err := config.ListContexts(paths)
		if err != nil {
			return cliError(ExitConfig, "Could not list Beam contexts.", "Check the Beam config directory.", err)
		}
		return renderer.Result(map[string]any{"current": paths.ContextName, "contexts": contexts}, func(w io.Writer) error {
			for _, name := range contexts {
				mark := " "
				if name == paths.ContextName {
					mark = "*"
				}
				_, _ = fmt.Fprintf(w, "%s %s\n", mark, name)
			}
			return nil
		})
	case "current":
		if len(args) != 1 {
			return usage("context current does not accept arguments")
		}
		name := firstNonEmpty(paths.ContextName, "default")
		return renderer.Result(map[string]string{"context": name}, func(w io.Writer) error { _, err := fmt.Fprintln(w, name); return err })
	case "create":
		if len(args) != 2 {
			return usage("context create requires one name")
		}
		if err := config.CreateContext(paths, args[1], cfg); err != nil {
			return cliError(ExitConflict, "Could not create the Beam context.", "Choose a new context name.", err)
		}
		return renderer.Result(map[string]any{"created": true, "context": args[1]}, func(w io.Writer) error { _, err := fmt.Fprintf(w, "Created context %s.\n", args[1]); return err })
	case "use":
		if len(args) != 2 {
			return usage("context use requires one name")
		}
		if err := config.UseContext(paths, args[1]); err != nil {
			return cliError(ExitNotFound, "Could not select the Beam context.", "Run `beam context list`.", err)
		}
		return renderer.Result(map[string]any{"active": true, "context": args[1]}, func(w io.Writer) error { _, err := fmt.Fprintf(w, "Using context %s.\n", args[1]); return err })
	case "show":
		if len(args) != 2 {
			return usage("context show requires one name")
		}
		selected, selectedPaths, err := config.LoadContext(args[1])
		if err != nil {
			return cliError(ExitNotFound, "Could not load the Beam context.", "Run `beam context list`.", err)
		}
		return renderConfig(selected, selectedPaths, renderer)
	case "delete":
		if len(args) != 2 {
			return usage("context delete requires one name")
		}
		if err := config.DeleteContext(paths, args[1]); err != nil {
			return cliError(ExitConflict, "Could not delete the Beam context.", "Switch away from an active context before deleting it.", err)
		}
		return renderer.Result(map[string]any{"deleted": true, "context": args[1]}, func(w io.Writer) error { _, err := fmt.Fprintf(w, "Deleted context %s.\n", args[1]); return err })
	default:
		return usage(fmt.Sprintf("unknown context command %q", args[0]))
	}
}

func (a *App) configCommand(_ context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "path":
		if len(args) != 1 {
			return usage("config path does not accept arguments")
		}
		if paths.Config == "" {
			paths, _ = config.ResolvePaths()
		}
		result := map[string]string{"directory": paths.Dir, "config": paths.Config, "credentials": paths.Credentials, "contexts": paths.Contexts, "active_context": paths.ActiveContext}
		return renderer.Result(result, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Config: %s\nCredentials: %s\nContexts: %s\n", paths.Config, paths.Credentials, paths.Contexts)
			return err
		})
	case "validate":
		if len(args) != 1 {
			return usage("config validate does not accept arguments")
		}
		loaded, loadedPaths, err := config.LoadContext(paths.ContextName)
		if err != nil {
			return cliError(ExitConfig, "Beam configuration is invalid.", err.Error(), err)
		}
		return renderer.Result(map[string]any{"valid": true, "context": loadedPaths.ContextName, "config": loaded}, func(w io.Writer) error { _, err := fmt.Fprintln(w, "Beam configuration is valid."); return err })
	case "list":
		if len(args) != 1 {
			return usage("config list does not accept arguments")
		}
		return renderConfig(cfg, paths, renderer)
	case "get":
		if len(args) != 2 {
			return usage("config get requires one key")
		}
		value, ok := configValue(cfg, args[1])
		if !ok {
			return usage("unknown configuration key " + args[1])
		}
		return renderer.Result(map[string]any{"key": args[1], "value": value}, func(w io.Writer) error { _, err := fmt.Fprintln(w, value); return err })
	case "set":
		if len(args) != 3 {
			return usage("config set requires a key and value")
		}
		if !setConfigValue(&cfg, args[1], args[2]) {
			return usage("unknown configuration key " + args[1])
		}
		if err := config.Save(paths, cfg); err != nil {
			return cliError(ExitConfig, "Could not save Beam configuration.", "Check directory permissions.", err)
		}
		return renderer.Result(map[string]any{"updated": true, "key": args[1], "value": args[2]}, func(w io.Writer) error { _, err := fmt.Fprintf(w, "%s=%s\n", args[1], args[2]); return err })
	case "unset":
		if len(args) != 2 {
			return usage("config unset requires one key")
		}
		if !unsetConfigValue(&cfg, paths, args[1]) {
			return usage("unknown configuration key " + args[1])
		}
		if err := config.Save(paths, cfg); err != nil {
			return cliError(ExitConfig, "Could not save Beam configuration.", "Check directory permissions.", err)
		}
		return renderer.Result(map[string]any{"unset": true, "key": args[1]}, func(w io.Writer) error { _, err := fmt.Fprintf(w, "Unset %s.\n", args[1]); return err })
	case "edit":
		if len(args) != 1 {
			return usage("config edit does not accept arguments")
		}
		path := paths.Config
		if paths.ContextConfig != "" {
			path = paths.ContextConfig
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := config.Save(paths, cfg); err != nil {
				return err
			}
		}
		editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
		if editor == "" {
			return cliError(ExitConfig, "No editor is configured.", "Set VISUAL or EDITOR.", nil)
		}
		parts := strings.Fields(editor)
		command := exec.Command(parts[0], append(parts[1:], path)...)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			return cliError(ExitOperationFailed, "The configuration editor failed.", "Edit the file returned by `beam config path`.", err)
		}
		return nil
	default:
		return usage(fmt.Sprintf("unknown config command %q", args[0]))
	}
}

func unsetConfigValue(cfg *config.Config, paths config.Paths, key string) bool {
	switch strings.ReplaceAll(key, "_", "-") {
	case "registry-url":
		cfg.RegistryURL = config.DefaultRegistryURL
	case "auth-url":
		cfg.AuthURL = config.DefaultAuthURL
	case "api-url":
		cfg.APIURL = config.DefaultAPIURL
	case "coordinator-url":
		cfg.CoordinatorURL = config.DefaultCoordinatorURL
	case "organization":
		cfg.Organization = ""
	case "agent-socket":
		cfg.AgentSocket = paths.AgentSocket
	case "output":
		cfg.Output = config.DefaultOutput
	default:
		return false
	}
	return true
}

func renderConfig(cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	result := map[string]any{"context": firstNonEmpty(paths.ContextName, "default"), "path": firstNonEmpty(paths.ContextConfig, paths.Config), "config": cfg}
	return renderer.Result(result, func(w io.Writer) error {
		encoded, _ := json.MarshalIndent(cfg, "", "  ")
		_, err := fmt.Fprintln(w, string(encoded))
		return err
	})
}

func configValue(cfg config.Config, key string) (string, bool) {
	switch strings.ReplaceAll(key, "_", "-") {
	case "registry-url":
		return cfg.RegistryURL, true
	case "auth-url":
		return cfg.AuthURL, true
	case "api-url":
		return cfg.APIURL, true
	case "coordinator-url":
		return cfg.CoordinatorURL, true
	case "organization":
		return cfg.Organization, true
	case "agent-socket":
		return cfg.AgentSocket, true
	case "output":
		return cfg.Output, true
	default:
		return "", false
	}
}

func setConfigValue(cfg *config.Config, key, value string) bool {
	switch strings.ReplaceAll(key, "_", "-") {
	case "registry-url":
		cfg.RegistryURL = value
	case "auth-url":
		cfg.AuthURL = value
	case "api-url":
		cfg.APIURL = value
	case "coordinator-url":
		cfg.CoordinatorURL = value
	case "organization":
		cfg.Organization = value
	case "agent-socket":
		cfg.AgentSocket = value
	case "output":
		cfg.Output = value
	default:
		return false
	}
	return true
}

func identityLabel(user *auth.User) string {
	if user == nil {
		return "unknown account"
	}
	return firstNonEmpty(user.Email, user.Name, user.ID, "unknown account")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
