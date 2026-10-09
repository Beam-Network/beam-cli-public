package command

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func (a *App) tunnelStudio(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return renderNamedHelp("studio", renderer)
	}

	switch args[0] {
	case "connect":
		request, err := parseStudioConnect(args[1:])
		if err != nil {
			return err
		}
		if request.EnrollmentCode == "" {
			if !renderer.Interactive {
				return usage("--code is required when input is not interactive")
			}
			if _, err := fmt.Fprint(renderer.Err, "Enrollment code: "); err != nil {
				return err
			}
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() || strings.TrimSpace(scanner.Text()) == "" {
				return usage("an enrollment code is required")
			}
			request.EnrollmentCode = strings.TrimSpace(scanner.Text())
		}
		status, err := client.ConnectStudio(ctx, request)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderStudioStatus(status, renderer)
	case "status":
		if len(args) != 1 {
			return usage("studio status does not accept arguments")
		}
		status, err := client.StudioConnection(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderStudioStatus(status, renderer)
	case "permissions":
		if len(args) != 1 {
			return usage("studio permissions does not accept arguments")
		}
		status, err := client.StudioConnection(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		if status.Permissions == nil {
			return cliError(ExitNotFound, "Studio permissions are unavailable.", "Connect this agent with `beam studio connect`.", nil)
		}
		return renderStudioPermissions(*status.Permissions, renderer)
	case "update-permissions":
		patch, err := parseStudioPermissionsPatch(args[1:])
		if err != nil {
			return err
		}
		status, err := client.UpdateStudioPermissions(ctx, patch)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderStudioStatus(status, renderer)
	case "disconnect":
		if len(args) != 1 {
			return usage("studio disconnect does not accept arguments")
		}
		status, err := client.DisconnectStudio(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderStudioStatus(status, renderer)
	default:
		return usage(fmt.Sprintf("unknown studio command %q", args[0]))
	}
}

func parseStudioPermissionsPatch(args []string) (tunnel.StudioPermissionsPatch, error) {
	var patch tunnel.StudioPermissionsPatch
	var roots, kinds, targets []string
	var rootsSet, kindsSet, targetsSet bool
	for index := 0; index < len(args); index++ {
		name, inline, hasInline := strings.Cut(args[index], "=")
		switch name {
		case "--clear-roots":
			if hasInline {
				return patch, usage(name + " does not accept a value")
			}
			roots, rootsSet = nil, true
			continue
		case "--clear-kinds":
			if hasInline {
				return patch, usage(name + " does not accept a value")
			}
			kinds, kindsSet = nil, true
			continue
		case "--clear-targets":
			if hasInline {
				return patch, usage(name + " does not accept a value")
			}
			targets, targetsSet = nil, true
			continue
		}
		value := func() (string, error) {
			if hasInline {
				if inline == "" {
					return "", usage(name + " requires a value")
				}
				return inline, nil
			}
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return "", usage(name + " requires a value")
			}
			index++
			return args[index], nil
		}
		raw, err := value()
		if err != nil {
			return patch, err
		}
		switch name {
		case "--allow-root":
			roots, rootsSet = append(roots, strings.TrimSpace(raw)), true
		case "--allow-kind":
			kinds, kindsSet = append(kinds, strings.TrimSpace(raw)), true
		case "--allow-target":
			targets, targetsSet = append(targets, strings.TrimSpace(raw)), true
		case "--allow-public":
			parsed, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				return patch, usage("--allow-public must be true or false")
			}
			patch.AllowPublic = &parsed
		case "--rooms":
			parsed, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				return patch, usage("--rooms must be true or false")
			}
			patch.RoomsEnabled = &parsed
		case "--max-tunnels", "--max-operations", "--max-command-ttl":
			parsed, parseErr := positiveStudioInt(name, raw)
			if parseErr != nil {
				return patch, parseErr
			}
			switch name {
			case "--max-tunnels":
				patch.MaxTunnels = &parsed
			case "--max-operations":
				patch.MaxConcurrentOperations = &parsed
			case "--max-command-ttl":
				patch.MaxCommandTTLSeconds = &parsed
			}
		default:
			return patch, usage("unknown option " + name)
		}
	}
	if rootsSet {
		patch.FilesystemRoots = &roots
	}
	if kindsSet {
		patch.TunnelKinds = &kinds
	}
	if targetsSet {
		patch.NetworkTargets = &targets
	}
	if len(args) == 0 {
		return patch, usage("studio update-permissions requires at least one option")
	}
	return patch, nil
}

func renderStudioPermissions(permissions tunnel.StudioPermissions, renderer output.Renderer) error {
	return renderer.Result(permissions, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Filesystem roots: %s\nNetwork targets: %s\nMax operations: %d\nMax command TTL: %ds\nRooms enabled: %t\n",
			strings.Join(permissions.FilesystemRoots, ", "), strings.Join(permissions.NetworkTargets, ", "),
			permissions.MaxConcurrentOperations, permissions.MaxCommandTTLSeconds, permissions.RoomsEnabled)
		return err
	})
}

func parseStudioConnect(args []string) (tunnel.StudioConnectionRequest, error) {
	request := tunnel.StudioConnectionRequest{
		MaxTunnels: 16, MaxConcurrentOperations: 4,
		MaxCommandTTLSeconds: 3600, RoomsEnabled: true,
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return request, usage("studio connect requires STUDIO_URL")
	}
	request.StudioURL = strings.TrimSpace(args[0])
	for index := 1; index < len(args); index++ {
		name, inline, hasInline := strings.Cut(args[index], "=")
		value := func() (string, error) {
			if hasInline {
				if inline == "" {
					return "", usage(name + " requires a value")
				}
				return inline, nil
			}
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return "", usage(name + " requires a value")
			}
			index++
			return args[index], nil
		}

		switch name {
		case "--allow-public":
			if hasInline {
				return request, usage("--allow-public does not accept a value")
			}
			request.AllowPublic = true
		case "--code", "--name", "--allow-root", "--allow-kind", "--allow-target", "--max-tunnels", "--max-operations", "--max-command-ttl", "--rooms":
			raw, err := value()
			if err != nil {
				return request, err
			}
			switch name {
			case "--code":
				request.EnrollmentCode = strings.TrimSpace(raw)
			case "--name":
				request.MachineName = strings.TrimSpace(raw)
			case "--allow-root":
				request.FilesystemRoots = append(request.FilesystemRoots, strings.TrimSpace(raw))
			case "--allow-kind":
				request.TunnelKinds = append(request.TunnelKinds, strings.TrimSpace(raw))
			case "--allow-target":
				request.NetworkTargets = append(request.NetworkTargets, strings.TrimSpace(raw))
			case "--max-tunnels":
				request.MaxTunnels, err = positiveStudioInt(name, raw)
			case "--max-operations":
				request.MaxConcurrentOperations, err = positiveStudioInt(name, raw)
			case "--max-command-ttl":
				request.MaxCommandTTLSeconds, err = positiveStudioInt(name, raw)
			case "--rooms":
				request.RoomsEnabled, err = strconv.ParseBool(raw)
				if err != nil {
					err = usage("--rooms must be true or false")
				}
			}
			if err != nil {
				return request, err
			}
		default:
			return request, usage("unknown option " + name)
		}
	}
	return request, nil
}

func positiveStudioInt(name, raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, usage(name + " must be a positive integer")
	}
	return value, nil
}

func renderStudioStatus(status tunnel.StudioConnectionStatus, renderer output.Renderer) error {
	return renderer.Result(status, func(w io.Writer) error {
		if !status.Configured {
			_, err := fmt.Fprintln(w, "Studio control is not configured.")
			return err
		}
		connection := "disconnected"
		if status.Connected {
			connection = "connected"
		}
		if _, err := fmt.Fprintf(w, "Studio: %s (%s)\n", status.StudioURL, connection); err != nil {
			return err
		}
		if status.AgentID != "" {
			_, err := fmt.Fprintf(w, "Agent: %s\n", status.AgentID)
			return err
		}
		return nil
	})
}

func studioHelpTopic() helpTopic {
	return helpTopic{
		Usage: []string{"beam studio <command>"},
		Sections: []helpSection{
			{Title: "Studio control commands", Entries: []helpEntry{
				{"connect STUDIO_URL [options]", "Enroll this beam-agentd in Beam Studio"},
				{"status", "Show the outbound Studio connection"},
				{"permissions", "Show the persisted local permission boundary"},
				{"update-permissions [options]", "Replace selected local permissions and reconnect"},
				{"disconnect", "Remove the local Studio enrollment"},
			}},
			{Title: "Connect options", Entries: []helpEntry{
				{"--code <code>", "One-time Studio enrollment code"},
				{"--name <name>", "Machine name shown in Studio"},
				{"--allow-root <path>", "Allowed filesystem root (repeatable)"},
				{"--allow-target <host>", "Allowed network target (repeatable)"},
				{"--max-operations <count>", "Maximum concurrent operations (default 4)"},
				{"--max-command-ttl <seconds>", "Maximum remote command TTL (default 3600)"},
				{"--rooms=<true|false>", "Permit room metadata control (default true)"},
			}},
		},
		Notes: []string{
			"Update options use the same allow/max/rooms flags. Boolean flags require an explicit true or false value. Repeated allow flags replace that allow-list; --clear-roots and --clear-targets remove a complete allow-list.",
		},
	}
}
