package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/beamapi"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func (a *App) agent(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return renderNamedHelp("agent", renderer)
	}
	client := tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version)
	var err error
	switch args[0] {
	case "connect":
		// Only `beam agent connect` itself honours BEAM_API_KEY; `beam setup`
		// keeps its account flow even when the variable is exported for rooms.
		return a.connectAgent(ctx, args[1:], os.Getenv("BEAM_API_KEY"), cfg, paths, client, renderer)
	case "start":
		if len(args) != 1 {
			return usage("agent start does not accept arguments")
		}
		result, err := a.startAgentDaemon(ctx, cfg, cfg.CoordinatorURL)
		if err != nil {
			return err
		}
		return renderer.Result(result, func(w io.Writer) error {
			if result.AlreadyRunning {
				_, err = fmt.Fprintf(w, "%s is already running\n", version.AgentName())
			} else {
				_, err = fmt.Fprintf(w, "%s started\n", version.AgentName())
			}
			return err
		})
	case "stop":
		if len(args) != 1 {
			return usage("agent stop does not accept arguments")
		}
		if err := ensureCompatible(ctx, client); err != nil {
			return err
		}
		if _, err := client.Shutdown(ctx); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(map[string]any{"stopping": true}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "%s stopping\n", version.AgentName())
			return err
		})
	case "restart":
		if len(args) != 1 {
			return usage("agent restart does not accept arguments")
		}
		coordinatorURL := cfg.CoordinatorURL
		if agentStatus, statusErr := client.AgentConnection(ctx); statusErr == nil && coordinatorURL == "" {
			coordinatorURL = agentStatus.CoordinatorURL
		}
		if _, statusErr := client.Status(ctx); statusErr == nil {
			if _, err := client.Shutdown(ctx); err != nil {
				return mapTunnelError(err)
			}
			if err := waitForAgentDaemonStop(ctx, client); err != nil {
				return err
			}
		}
		result, err := a.startAgentDaemon(ctx, cfg, coordinatorURL)
		if err != nil {
			return err
		}
		return renderer.Result(result, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "%s restarted\n", version.AgentName())
			return err
		})
	case "status":
		if len(args) != 1 {
			return usage("agent status does not accept arguments")
		}
		// Status only reports. Starting the daemon here would undo an
		// explicit `agent stop`, so a stopped daemon is reported as stopped.
		daemonStatus, err := client.Status(ctx)
		if daemonStopped(err) {
			return renderAgentStopped(renderer)
		}
		if err != nil {
			return mapTunnelError(err)
		}
		if !tunnel.Compatible(daemonStatus) {
			return cliError(ExitVersionIncompatible, "beam-agentd is incompatible with this CLI.", "Run `beam update`.", tunnel.VersionError(daemonStatus))
		}
		status, err := client.AgentConnection(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderAgentStatus(status, renderer)
	case "dashboard":
		if err := validateOptions(args[1:], nil, map[string]bool{"--no-open": true}); err != nil {
			return err
		}
		if len(args) > 2 || (len(args) == 2 && args[1] != "--no-open") {
			return usage("agent dashboard accepts only --no-open")
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		session, err := client.StartDashboard(ctx)
		if err != nil {
			var tunnelErr *tunnel.Error
			if errors.As(err, &tunnelErr) && (tunnelErr.Status == 404 || tunnelErr.Status == 501) {
				return cliError(ExitVersionIncompatible, "This beam-agentd does not support the web dashboard.", "Run `beam update` and retry `beam agent dashboard`.", err)
			}
			return mapTunnelError(err)
		}
		opened := false
		if renderer.Mode == output.Human && renderer.Interactive && !has(args[1:], "--no-open") && a.openURL != nil {
			opened = a.openURL(session.URL) == nil
		}
		return renderer.Result(session, func(w io.Writer) error {
			if opened {
				_, err = fmt.Fprintln(w, "Dashboard opened in your browser.")
			} else {
				_, err = fmt.Fprintf(w, "Dashboard ready: %s\n", session.URL)
			}
			return err
		})
	case "credentials":
		if len(args) != 2 || args[1] != "recover" {
			return usage("agent credentials requires recover")
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		status, err := client.RecoverAgent(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(status, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "✓ Credentials recovered for %s.\n", status.AgentID)
			return err
		})
	case "repair":
		if len(args) != 1 {
			return usage("agent repair does not accept arguments")
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		status, err := client.RecoverAgent(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(status, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "✓ Agent %s repaired.\n", status.AgentID)
			return err
		})
	case "logs":
		if err := validateOptions(args[1:], nil, map[string]bool{"--follow": true}); err != nil {
			return err
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		return tunnelLogs(ctx, args[1:], client, renderer)
	case "disconnect":
		if len(args) != 1 {
			return usage("agent disconnect does not accept arguments")
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		status, err := client.DisconnectAgent(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		if _, err := client.Shutdown(ctx); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(status, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Agent disconnected locally and %s is stopping. The remote agent record was not revoked.\n", version.AgentName())
			return err
		})
	case "rename":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return usage("agent rename requires one label")
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		status, err := client.AgentConnection(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		if !status.Registered {
			return usage("agent must be registered before it can be renamed")
		}
		session, err := a.ensureAgentUserSession(ctx, cfg, paths, renderer)
		if err != nil {
			return err
		}
		managed, err := manageAgentIdentity(ctx, http.MethodPatch, status, map[string]any{"label": strings.TrimSpace(args[1])}, session)
		if err != nil {
			return err
		}
		return renderer.Result(managed, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "✓ Agent %s renamed to %q.\n", managed.AgentID, managed.Label)
			return err
		})
	case "revoke":
		if err := validateOptions(args[1:], nil, map[string]bool{"--yes": true}); err != nil {
			return err
		}
		if len(args) != 2 || !has(args[1:], "--yes") {
			return usage("agent revoke requires --yes")
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		status, err := client.AgentConnection(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		if !status.Registered {
			return usage("agent must be registered before it can be revoked")
		}
		session, err := a.ensureAgentUserSession(ctx, cfg, paths, renderer)
		if err != nil {
			return err
		}
		managed, err := manageAgentIdentity(ctx, http.MethodDelete, status, nil, session)
		if err != nil {
			return err
		}
		if _, err := client.MarkAgentRevoked(ctx); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(managed, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Agent %s revoked.\n", managed.AgentID)
			return err
		})
	case "diagnostics":
		if len(args) != 1 {
			return usage("agent diagnostics does not accept arguments")
		}
		client, err = a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		daemonStatus, err := client.Status(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		agentStatus, err := client.AgentConnection(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		metrics, err := client.Metrics(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		result := map[string]any{"daemon": daemonStatus, "agent": agentStatus, "metrics": metrics}
		return renderer.Result(result, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "daemon_ready=%t agent_state=%s agent_id=%s last_error=%q\n", daemonStatus.Ready, agentStatus.State, agentStatus.AgentID, agentStatus.LastError)
			return err
		})
	default:
		return usage(fmt.Sprintf("unknown agent command %q", args[0]))
	}
}

type managedAgentIdentity struct {
	AgentID        string    `json:"agent_id"`
	OrganizationID string    `json:"organization_id"`
	Label          string    `json:"label"`
	RevokedAt      time.Time `json:"revoked_at,omitempty"`
}

func (a *App) ensureAgentUserSession(ctx context.Context, cfg config.Config, paths config.Paths, renderer output.Renderer) (*auth.Session, error) {
	session := &auth.Session{Client: auth.DeviceClient{BaseURL: cfg.AuthURL}, Store: a.authStore(paths.Credentials)}
	hasSession, err := session.HasSession()
	if err != nil {
		return nil, cliError(ExitConfig, "Could not read Beam credentials.", "Check the OS credential store and Beam config directory permissions.", err)
	}
	if !hasSession {
		if !renderer.Interactive {
			hint := "Run `beam login` first."
			if a.noInteractive {
				hint = "Run `beam login` first or remove `--no-interactive` in a terminal."
			}
			return nil, cliError(ExitAuth, "Beam login is required.", hint, nil)
		}
		loginRenderer := renderer
		loginRenderer.Out = io.Discard
		if err := a.authLogin(ctx, cfg, session, loginRenderer); err != nil {
			return nil, err
		}
	}
	return session, nil
}

func manageAgentIdentity(ctx context.Context, method string, status tunnel.AgentConnectionStatus, body any, session *auth.Session) (managedAgentIdentity, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return managedAgentIdentity{}, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := session.AccessToken(ctx, attempt > 0)
		if err != nil {
			return managedAgentIdentity{}, cliError(ExitAuth, "Beam authentication is missing or expired.", `Run "beam auth login".`, err)
		}
		request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(status.CoordinatorURL, "/")+"/v1/agents/"+status.AgentID, bytes.NewReader(payload))
		if err != nil {
			return managedAgentIdentity{}, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Beam-Organization-ID", status.OrganizationID)
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
		if err != nil {
			return managedAgentIdentity{}, cliError(ExitOperationFailed, "Could not contact the Beam coordinator.", "Check the coordinator connection and try again.", err)
		}
		var result struct {
			Agent managedAgentIdentity `json:"agent"`
			Error string               `json:"error"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
		_ = response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return managedAgentIdentity{}, cliError(ExitOperationFailed, "Coordinator rejected the agent management request.", result.Error, nil)
		}
		if decodeErr != nil || result.Agent.AgentID == "" {
			return managedAgentIdentity{}, cliError(ExitOperationFailed, "Coordinator returned an invalid agent response.", "Try again.", decodeErr)
		}
		return result.Agent, nil
	}
	return managedAgentIdentity{}, cliError(ExitAuth, "Beam authentication was rejected by the coordinator.", `Run "beam auth login" and try again.`, nil)
}

func (a *App) agentConnect(ctx context.Context, args []string, cfg config.Config, paths config.Paths, client *tunnel.LocalClient, renderer output.Renderer) error {
	return a.connectAgent(ctx, args, "", cfg, paths, client, renderer)
}

// connectAgent registers this machine. The enrollment is minted, in order of
// precedence, from --enrollment-token, from a Beam API key (--api-key, else
// environmentAPIKey), or from the Beam account session.
func (a *App) connectAgent(ctx context.Context, args []string, environmentAPIKey string, cfg config.Config, paths config.Paths, client *tunnel.LocalClient, renderer output.Renderer) error {
	if err := validateOptions(args, map[string]bool{
		"--coordinator": true, "--organization": true, "--label": true, "--enrollment-token": true, "--api-key": true,
	}, nil); err != nil {
		return err
	}
	enrollmentToken, enrollmentTokenProvided, err := flag(args, "--enrollment-token")
	if err != nil {
		return err
	}
	apiKey, apiKeyProvided, err := flag(args, "--api-key")
	if err != nil {
		return err
	}
	if apiKeyProvided && strings.TrimSpace(apiKey) == "" {
		return usage("--api-key cannot be empty")
	}
	if apiKeyProvided && enrollmentTokenProvided {
		return usage("--api-key and --enrollment-token cannot be combined")
	}
	if !apiKeyProvided && !enrollmentTokenProvided {
		apiKey = environmentAPIKey
	}
	apiKey = strings.TrimSpace(apiKey)
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usage("agent connect does not accept positional arguments")
	}
	coordinatorURL, coordinatorProvided, err := flag(args, "--coordinator")
	if err != nil {
		return err
	}
	explicitOrganization, organizationProvided, err := flag(args, "--organization")
	if err != nil {
		return err
	}
	if organizationProvided && strings.TrimSpace(explicitOrganization) == "" {
		return usage("--organization cannot be empty")
	}
	var knownOrganizations []auth.Organization
	if organizationProvided {
		knownOrganizations = cachedOrganizations(paths)
	}
	if coordinatorURL == "" {
		coordinatorURL = cfg.CoordinatorURL
	}
	if existing, statusErr := client.AgentConnection(ctx); statusErr == nil {
		if existing.Registered {
			if err := validateRegisteredAgentSelection(existing, coordinatorURL, coordinatorProvided, explicitOrganization, organizationProvided, knownOrganizations); err != nil {
				return err
			}
			selectedCoordinator := existing.CoordinatorURL
			if selectedCoordinator == "" {
				selectedCoordinator = coordinatorURL
			}
			if err := saveAgentSelection(paths, cfg, selectedCoordinator, existing.OrganizationID); err != nil {
				return err
			}
			return renderAgentStatus(existing, renderer)
		}
		if coordinatorURL == "" {
			coordinatorURL = existing.CoordinatorURL
		}
	}
	if coordinatorURL == "" {
		return usage("--coordinator is required until a default coordinator is configured with BEAM_COORDINATOR_URL")
	}
	if _, err := a.startAgentDaemon(ctx, cfg, coordinatorURL); err != nil {
		return err
	}
	prepared, err := client.PrepareAgentConnection(ctx, coordinatorURL)
	if err != nil {
		return mapTunnelError(err)
	}
	if !prepared.RevokedAt.IsZero() || strings.EqualFold(prepared.State, "revoked") {
		return typedCLIError(ExitConflict, errorKindAgentRevoked, "This machine's Beam agent has been revoked.", "Run `beam agent repair` or `beam agent disconnect` before onboarding again.", nil)
	}
	if prepared.Registered {
		if err := validateRegisteredAgentSelection(prepared, coordinatorURL, coordinatorProvided, explicitOrganization, organizationProvided, knownOrganizations); err != nil {
			return err
		}
		selectedCoordinator := prepared.CoordinatorURL
		if selectedCoordinator == "" {
			selectedCoordinator = coordinatorURL
		}
		if err := saveAgentSelection(paths, cfg, selectedCoordinator, prepared.OrganizationID); err != nil {
			return err
		}
		return renderAgentStatus(prepared, renderer)
	}
	selectedOrganizationID := ""
	if enrollmentToken == "" && apiKey != "" {
		enrollmentToken, selectedOrganizationID, err = a.mintAgentEnrollmentWithAPIKey(ctx, args, cfg, paths, coordinatorURL, prepared.PublicKeyFingerprint, apiKey, explicitOrganization, renderer)
		if err != nil {
			return err
		}
	}
	if enrollmentToken == "" {
		organization := explicitOrganization
		if strings.TrimSpace(organization) == "" {
			organization = cfg.Organization
		}
		session, err := a.ensureAgentUserSession(ctx, cfg, paths, renderer)
		if err != nil {
			return err
		}
		api := beamapi.Client{BaseURL: cfg.APIURL, Tokens: session}
		user, organizations, err := api.Context(ctx)
		if err != nil && renderer.Interactive && beamAPIAuthenticationError(err) {
			renderer.Progress("Your Beam session has expired. Sign in again to continue.")
			loginRenderer := renderer
			loginRenderer.Out = io.Discard
			if loginErr := a.authLogin(ctx, cfg, session, loginRenderer); loginErr != nil {
				return loginErr
			}
			user, organizations, err = api.Context(ctx)
		}
		if err != nil {
			return mapBeamAPIError(err)
		}
		if err := session.Store.UpdateContext(user, organizations); err != nil {
			return cliError(ExitConfig, "Could not save the refreshed Beam account context.", "Check the Beam config directory permissions.", err)
		}
		if user != nil {
			status := strings.ToUpper(strings.TrimSpace(user.RestrictionStatus))
			if status != "" && status != "NONE" {
				return typedCLIError(ExitAuth, errorKindAccountUnavailable, "This Beam account cannot enroll a machine.", "Resolve the account restriction in Beam before running setup again. Current status: "+status+".", nil)
			}
		}
		selected, err := selectAgentOrganization(organizations, organization)
		// A saved organization can disappear or become inaccessible. It is a
		// preference, not an instruction, so fall back to the current list.
		if err != nil && !organizationProvided && strings.TrimSpace(organization) != "" {
			selected, err = selectAgentOrganization(organizations, "")
		}
		if err != nil && renderer.Interactive && asCLIError(err).Kind == errorKindOrganizationSelection {
			selected, err = a.promptOrganization(organizations, renderer)
		}
		if err != nil {
			return err
		}
		label, err := a.agentLabel(args, renderer)
		if err != nil {
			return err
		}
		selectedOrganizationID = selected.ID
		enrollmentToken, err = createAgentEnrollment(ctx, coordinatorURL, label, prepared.PublicKeyFingerprint, selected.ID, session)
		if err != nil {
			return err
		}
	}
	status, err := client.ConnectAgent(ctx, tunnel.AgentConnectionRequest{CoordinatorURL: coordinatorURL, EnrollmentToken: enrollmentToken})
	if err != nil {
		return mapTunnelError(err)
	}
	// Restart once after first enrollment so every runtime subsystem is born
	// with the daemon-owned credential; subsequent starts load it directly.
	if _, err := client.Shutdown(ctx); err != nil {
		return mapTunnelError(err)
	}
	if err := waitForAgentDaemonStop(ctx, client); err != nil {
		return err
	}
	if _, err := a.startAgentDaemon(ctx, cfg, coordinatorURL); err != nil {
		return err
	}
	if refreshed, refreshErr := client.AgentConnection(ctx); refreshErr == nil {
		status = refreshed
	}
	if status.OrganizationID != "" {
		selectedOrganizationID = status.OrganizationID
	}
	if err := saveAgentSelection(paths, cfg, coordinatorURL, selectedOrganizationID); err != nil {
		return err
	}
	return renderer.Result(status, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "✓ Agent %s registered as %q.\n", status.AgentID, status.Label)
		return err
	})
}

// agentLabel returns --label, else the hostname, letting an interactive user
// confirm or change it.
func (a *App) agentLabel(args []string, renderer output.Renderer) (string, error) {
	label, _, err := flag(args, "--label")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(label) == "" {
		label, _ = os.Hostname()
		if renderer.Interactive {
			for {
				label, err = a.promptText(renderer, "Machine name", label, true)
				if err != nil {
					return "", err
				}
				if normalized, labelErr := validAgentLabel(label); labelErr == nil {
					return normalized, nil
				} else {
					renderer.Progress(labelErr.Error())
				}
			}
		}
	}
	return validAgentLabel(label)
}

func beamAPIAuthenticationError(err error) bool {
	var apiErr *beamapi.Error
	return errors.As(err, &apiErr) && apiErr.Kind == beamapi.ErrAuth
}

// validateRegisteredAgentSelection refuses a setup that would silently change
// a registered machine's coordinator or organization. --organization may name
// the registered organization by ID, or by a slug or public ID that the
// organizations cached at the last Beam login resolve to that ID.
func validateRegisteredAgentSelection(status tunnel.AgentConnectionStatus, coordinator string, coordinatorProvided bool, organization string, organizationProvided bool, organizations []auth.Organization) error {
	if !status.RevokedAt.IsZero() || strings.EqualFold(status.State, "revoked") {
		return typedCLIError(ExitConflict, errorKindAgentRevoked, "This machine's Beam agent has been revoked.", "Run `beam agent repair` or `beam agent disconnect` before onboarding again.", nil)
	}
	if status.RoomJoinOnly {
		return typedCLIError(ExitConflict, errorKindAgentConflict, "This machine is registered through a Room invitation.", "Run `beam agent disconnect` before connecting it to a Beam account.", nil)
	}
	if coordinatorProvided && status.CoordinatorURL != "" && !sameEndpoint(status.CoordinatorURL, coordinator) {
		return typedCLIError(ExitConflict, errorKindAgentConflict, "This machine is registered with another coordinator.", "Run `beam agent disconnect` before changing coordinators.", nil)
	}
	if organizationProvided && status.OrganizationID != "" && !namesOrganization(organization, status.OrganizationID, organizations) {
		return typedCLIError(ExitConflict, errorKindAgentConflict, "This machine is registered with another organization.", "Use its organization ID or run `beam agent disconnect` before changing organizations.", nil)
	}
	return nil
}

// namesOrganization reports whether requested is organizationID itself or a
// slug or public ID that organizations resolve to it.
func namesOrganization(requested, organizationID string, organizations []auth.Organization) bool {
	requested = strings.TrimSpace(requested)
	if requested == organizationID {
		return true
	}
	organization, found := findOrganization(organizations, requested)
	return found && organization.ID == organizationID
}

// cachedOrganizations returns the organization list saved at the last Beam
// login, or nil without one. A file-only store: the list lives in the
// credentials file, so reading it never needs the OS credential store.
func cachedOrganizations(paths config.Paths) []auth.Organization {
	credentials, err := (auth.Store{Path: paths.Credentials}).Load()
	if err != nil {
		return nil
	}
	return credentials.Organizations
}

func sameEndpoint(left, right string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(left), "/"), strings.TrimRight(strings.TrimSpace(right), "/"))
}

func validAgentLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", usage("machine name is required")
	}
	if !utf8.ValidString(label) {
		return "", usage("machine name must be valid UTF-8")
	}
	if utf8.RuneCountInString(label) > 128 {
		return "", usage("machine name must be at most 128 characters")
	}
	for _, character := range label {
		if unicode.IsControl(character) {
			return "", usage("machine name must not contain control characters")
		}
	}
	return label, nil
}

func saveAgentSelection(paths config.Paths, cfg config.Config, coordinatorURL, organizationID string) error {
	cfg.CoordinatorURL = coordinatorURL
	if organizationID != "" {
		cfg.Organization = organizationID
	}
	if err := config.Save(paths, cfg); err != nil {
		return cliError(ExitConfig, "Agent connected, but its configuration could not be saved.", "Check the Beam config directory permissions.", err)
	}
	return nil
}

func waitForAgentDaemonStop(ctx context.Context, client *tunnel.LocalClient) error {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return cliError(ExitDaemonUnavailable, "Agent did not finish stopping.", "Inspect BEAM_AGENT_LOG and retry the command.", ctx.Err())
		case <-ticker.C:
			if _, err := client.Status(ctx); err != nil {
				if err := tunnel.WaitForDaemonOwnershipRelease(ctx, client.SocketPath); err != nil {
					return cliError(ExitDaemonUnavailable, "Agent did not finish releasing its socket.", "Inspect BEAM_AGENT_LOG and retry the command.", err)
				}
				return nil
			}
		}
	}
}

func (a *App) startAgentDaemon(ctx context.Context, cfg config.Config, coordinatorURL string) (tunnel.StartResult, error) {
	result, err := a.daemonStart(ctx, tunnel.StartOptions{
		SocketPath: cfg.AgentSocket, CoordinatorURL: coordinatorURL, AgentBinary: os.Getenv("BEAM_TUNNEL_AGENT_BINARY"),
		CLIVersion: a.version.Version, LogPath: os.Getenv("BEAM_AGENT_LOG"),
	})
	if err != nil {
		var tunnelErr *tunnel.Error
		if errors.As(err, &tunnelErr) && tunnelErr.Kind == tunnel.ErrVersion {
			return tunnel.StartResult{}, mapTunnelError(err)
		}
		return tunnel.StartResult{}, cliError(ExitDaemonUnavailable, "Could not start beam-agentd.", "Install the matching Beam bundle and inspect BEAM_AGENT_LOG if startup fails.", err)
	}
	return result, nil
}

// daemonStopped reports whether err means nothing is listening on the local
// daemon socket.
func daemonStopped(err error) bool {
	var tunnelErr *tunnel.Error
	return errors.As(err, &tunnelErr) && tunnelErr.Kind == tunnel.ErrUnavailable
}

// agentStoppedStatus is the status of an agent whose daemon is not running.
// Its fields mirror tunnel.AgentConnectionStatus so a script reads the same
// state and connected fields either way.
type agentStoppedStatus struct {
	State         string `json:"state"`
	Connected     bool   `json:"connected"`
	DaemonRunning bool   `json:"daemon_running"`
}

func renderAgentStopped(renderer output.Renderer) error {
	return renderer.Result(agentStoppedStatus{State: "stopped"}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s is stopped. %s\n", version.AgentName(), applyReleaseNames("Run `beam agent start` to start it."))
		return err
	})
}

func renderAgentStatus(status tunnel.AgentConnectionStatus, renderer output.Renderer) error {
	return renderer.Result(status, func(w io.Writer) error {
		if !status.Registered {
			_, err := fmt.Fprintln(w, applyReleaseNames("Agent is not registered. Run `beam agent connect`."))
			return err
		}
		_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", status.AgentID, status.Label, status.State, status.CoordinatorURL)
		return err
	})
}

func selectAgentOrganization(organizations []auth.Organization, requested string) (auth.Organization, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, organization := range organizations {
			if organization.ID == requested || organization.PublicID == requested || organization.Slug == requested {
				if !organizationAvailable(organization) {
					return auth.Organization{}, organizationUnavailableError([]auth.Organization{organization})
				}
				return organization, nil
			}
		}
		return auth.Organization{}, typedCLIError(ExitNotFound, errorKindOrganizationNotFound, "The selected Beam organization is no longer accessible.", accessibleOrganizationHint(organizations), nil)
	}
	available := availableOrganizations(organizations)
	if len(organizations) == 0 {
		return auth.Organization{}, typedCLIError(ExitNotFound, errorKindOrganizationRequired, "No Beam organization is associated with this account.", "Create or join an organization, then run `beam setup` again.", nil)
	}
	if len(available) == 0 {
		return auth.Organization{}, organizationUnavailableError(organizations)
	}
	if len(available) == 1 {
		return available[0], nil
	}
	for _, organization := range available {
		if organization.IsDefault {
			return organization, nil
		}
	}
	return auth.Organization{}, typedCLIError(ExitUsage, errorKindOrganizationSelection, "Choose a Beam organization for this machine.", "Pass --organization with an accessible organization ID or slug.", nil)
}

func accessibleOrganizationHint(organizations []auth.Organization) string {
	available := availableOrganizations(organizations)
	if len(available) == 0 {
		return "Create or join an organization, then run `beam setup` again."
	}
	names := make([]string, 0, len(available))
	for _, organization := range available {
		names = append(names, firstNonEmpty(organization.Slug, organization.PublicID, organization.ID))
	}
	return "Choose an accessible organization: " + strings.Join(names, ", ") + "."
}

func organizationAvailable(organization auth.Organization) bool {
	return normalizedRestriction(organization) == "NONE"
}

func normalizedRestriction(organization auth.Organization) string {
	status := strings.ToUpper(strings.TrimSpace(organization.RestrictionStatus))
	if status == "" {
		return "NONE"
	}
	return status
}

func availableOrganizations(organizations []auth.Organization) []auth.Organization {
	result := make([]auth.Organization, 0, len(organizations))
	for _, organization := range organizations {
		if organizationAvailable(organization) {
			result = append(result, organization)
		}
	}
	return result
}

func organizationUnavailableError(organizations []auth.Organization) error {
	statuses := make([]string, 0, len(organizations))
	for _, organization := range organizations {
		status := normalizedRestriction(organization)
		if status != "NONE" {
			statuses = append(statuses, status)
		}
	}
	detail := ""
	if len(statuses) != 0 {
		detail = " Current restriction status: " + strings.Join(statuses, ", ") + "."
	}
	return typedCLIError(ExitAuth, errorKindOrganizationUnavailable, "No accessible Beam organization can enroll this machine.", "Resolve the organization restriction or choose another organization."+detail, nil)
}

func createAgentEnrollment(ctx context.Context, coordinatorURL, label, fingerprint, organizationID string, session *auth.Session) (string, error) {
	body, err := json.Marshal(map[string]any{"label": label, "public_key_fingerprint": fingerprint})
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := session.AccessToken(ctx, attempt > 0)
		if err != nil {
			return "", cliError(ExitAuth, "Beam authentication is missing or expired.", `Run "beam auth login".`, err)
		}
		response, err := postAgentEnrollment(ctx, coordinatorURL, token, organizationID, body)
		if err != nil {
			return "", err
		}
		result := response.body
		decodeErr := response.decodeErr
		if response.statusCode == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if response.statusCode < 200 || response.statusCode >= 300 {
			switch response.statusCode {
			case http.StatusForbidden:
				return "", typedCLIError(ExitAuth, errorKindOrganizationUnavailable, "The selected organization cannot enroll this machine.", result.Error, nil)
			case http.StatusNotFound:
				return "", typedCLIError(ExitNotFound, errorKindOrganizationNotFound, "The selected organization is no longer available.", "Refresh your organizations and run `beam setup` again.", nil)
			case http.StatusConflict:
				return "", typedCLIError(ExitConflict, errorKindAgentConflict, "The coordinator rejected this machine because it conflicts with existing state.", result.Error, nil)
			}
			return "", cliError(ExitOperationFailed, "Agent enrollment was rejected by the coordinator.", result.Error, nil)
		}
		if decodeErr != nil || result.EnrollmentToken == "" {
			return "", cliError(ExitOperationFailed, "Coordinator returned an invalid enrollment response.", "Try again; no agent credential was stored by the CLI.", decodeErr)
		}
		return result.EnrollmentToken, nil
	}
	return "", cliError(ExitAuth, "Beam authentication was rejected by the coordinator.", `Run "beam auth login" and try again.`, nil)
}

type agentEnrollmentResponse struct {
	statusCode int
	body       struct {
		EnrollmentToken string `json:"enrollment_token"`
		Error           string `json:"error"`
	}
	decodeErr error
}

// postAgentEnrollment asks the coordinator for a one-time enrollment bound to
// this machine's key fingerprint. bearer is either an account access token or
// an organization Beam API key; the coordinator accepts both on this route.
// Only transport failures are returned as errors, so callers map statuses.
func postAgentEnrollment(ctx context.Context, coordinatorURL, bearer, organizationID string, body []byte) (agentEnrollmentResponse, error) {
	var result agentEnrollmentResponse
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(coordinatorURL, "/")+"/v1/agent-enrollments", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("X-Beam-Organization-ID", organizationID)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return result, cliError(ExitOperationFailed, "Could not contact the Beam coordinator.", "Check BEAM_COORDINATOR_URL and your network connection.", err)
	}
	result.statusCode = response.StatusCode
	result.decodeErr = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result.body)
	_ = response.Body.Close()
	if result.body.Error == "" && (response.StatusCode < 200 || response.StatusCode >= 300) {
		result.body.Error = response.Status
	}
	return result, nil
}

// apiKeyEnrollmentHint names what a Beam API key needs to enroll a machine.
// The coordinator accepts any active key of the organization, but the Beam API
// only recognises keys whose role grants at least one billable action.
const apiKeyEnrollmentHint = "Use an active API key of the organization whose role grants at least one billable action " +
	"(rooms:start, rooms:create, transfers:create, workflows:run, or actions:execute). " +
	"Create or inspect keys in the Beam Console."

// mintAgentEnrollmentWithAPIKey obtains an enrollment without a Beam login, for
// headless machines and automation. The key never leaves this process except
// as the Authorization header of the Beam API and coordinator requests.
func (a *App) mintAgentEnrollmentWithAPIKey(ctx context.Context, args []string, cfg config.Config, paths config.Paths, coordinatorURL, fingerprint, apiKey, requestedOrganization string, renderer output.Renderer) (string, string, error) {
	organizationID, err := apiKeyOrganization(ctx, cfg, paths, apiKey, requestedOrganization)
	if err != nil {
		return "", "", err
	}
	label, err := a.agentLabel(args, renderer)
	if err != nil {
		return "", "", err
	}
	renderer.Progress(fmt.Sprintf("Enrolling this machine in organization %s with a Beam API key.", organizationID))
	body, err := json.Marshal(map[string]any{"label": label, "public_key_fingerprint": fingerprint})
	if err != nil {
		return "", "", err
	}
	response, err := postAgentEnrollment(ctx, coordinatorURL, apiKey, organizationID, body)
	if err != nil {
		return "", "", err
	}
	switch {
	case response.statusCode >= 200 && response.statusCode < 300:
		if response.decodeErr != nil || response.body.EnrollmentToken == "" {
			return "", "", cliError(ExitOperationFailed, "Coordinator returned an invalid enrollment response.", "Try again; no agent credential was stored by the CLI.", response.decodeErr)
		}
		return response.body.EnrollmentToken, organizationID, nil
	case response.statusCode == http.StatusUnauthorized:
		return "", "", typedCLIError(ExitAuth, errorKindAPIKeyRejected, "The coordinator rejected the Beam API key.", apiKeyEnrollmentHint, nil)
	case response.statusCode == http.StatusForbidden:
		return "", "", typedCLIError(ExitAuth, errorKindOrganizationUnavailable,
			fmt.Sprintf("The Beam API key does not belong to organization %s.", organizationID),
			"Omit --organization to use the key's own organization, or pass a key issued for this one.", nil)
	case response.statusCode == http.StatusServiceUnavailable:
		return "", "", cliError(ExitOperationFailed, "This coordinator does not offer agent enrollment.", "Check BEAM_COORDINATOR_URL, or ask the Beam operator to enable enrollment. Coordinator said: "+response.body.Error, nil)
	default:
		return "", "", cliError(ExitOperationFailed, "Agent enrollment was rejected by the coordinator.", response.body.Error, nil)
	}
}

// apiKeyOrganization returns the organization ID an enrollment is minted for.
// The key is always checked with the Beam API first, so a revoked, expired, or
// otherwise unusable key reports the Beam API's reason whether or not
// --organization is given. The key can only enroll into its own organization,
// so --organization acts as a guard: it must name that organization, either by
// ID or, on a machine with a Beam login, by a slug or public ID from the cached
// organization list. The Beam API does not report a key's slug, so without a
// login a slug cannot be confirmed and the ID is required.
func apiKeyOrganization(ctx context.Context, cfg config.Config, paths config.Paths, apiKey, requested string) (string, error) {
	identity, err := beamapi.LookupAPIKeyOrganization(ctx, cfg.APIURL, apiKey, nil)
	var rejected *beamapi.APIKeyRejectedError
	if errors.As(err, &rejected) {
		hint := apiKeyEnrollmentHint
		if rejected.Message != "" {
			hint = "Beam API: " + strings.TrimSuffix(rejected.Message, ".") + ". " + hint
		}
		return "", typedCLIError(ExitAuth, errorKindAPIKeyRejected, "The Beam API key cannot enroll this machine.", hint, nil)
	}
	if err != nil {
		return "", cliError(ExitOperationFailed, "Could not determine which organization the Beam API key belongs to.", "Check network access to the Beam API (BEAM_API_URL) and try again.", err)
	}
	keyOrganization := identity.OrganizationID
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == keyOrganization {
		return keyOrganization, nil
	}
	// A file-only store: the cached organization list lives in the credentials
	// file, so reading it never needs the OS credential store.
	if credentials, err := (auth.Store{Path: paths.Credentials}).Load(); err == nil {
		if organization, found := findOrganization(credentials.Organizations, requested); found {
			if organization.ID == keyOrganization {
				return keyOrganization, nil
			}
			return "", typedCLIError(ExitAuth, errorKindOrganizationUnavailable,
				fmt.Sprintf("The Beam API key does not belong to organization %s.", requested),
				fmt.Sprintf("The key belongs to organization %s. Omit --organization to use it, or pass a key issued for %s.", keyOrganization, requested), nil)
		}
	}
	hint := fmt.Sprintf("The key belongs to organization %s. Pass that organization ID, or omit --organization to use it.", keyOrganization)
	if !looksLikeOrganizationID(requested) {
		hint += " Organization slugs and public IDs are resolved only after `beam login` on this machine."
	}
	return "", typedCLIError(ExitAuth, errorKindOrganizationUnavailable,
		fmt.Sprintf("Could not match organization %q to the Beam API key's organization.", requested), hint, nil)
}

// organizationIDPattern matches the shapes of a Beam organization ID: a cuid
// (the Beam API's IDs), a UUID, or an org_-prefixed ID.
var organizationIDPattern = regexp.MustCompile(`^(c[a-z0-9]{24}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|org_[A-Za-z0-9_-]+)$`)

// looksLikeOrganizationID reports whether value is shaped like an organization
// ID rather than a slug or public ID, so a hint about resolving slugs after a
// login is shown only when it can help.
func looksLikeOrganizationID(value string) bool {
	return organizationIDPattern.MatchString(value)
}
