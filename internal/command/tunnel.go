package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func (a *App) tunnel(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return renderNamedHelp("tunnel", renderer)
	}
	var client tunnel.Client = tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version)
	if tunnelCommandUsesDaemon(args[0]) {
		client = a.autoStartingLocalClient(cfg)
	}
	switch args[0] {
	case "start":
		return tunnelStart(ctx, args[1:], cfg, a.version.Version, renderer)
	case "stop":
		if len(args) == 2 {
			return a.unshare(ctx, args[1:], cfg, paths, renderer)
		}
		if len(args) != 1 {
			return usage("tunnel stop accepts at most one tunnel name or ID")
		}
		if err := ensureCompatible(ctx, client); err != nil {
			return err
		}
		result, err := client.Shutdown(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(result, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "%s stopping\n", version.AgentName())
			return err
		})
	case "status":
		if len(args) != 1 {
			return usage("tunnel status does not accept arguments")
		}
		status, err := client.Status(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		if !tunnel.Compatible(status) {
			return cliError(ExitVersionIncompatible, "beam-agentd is incompatible with this CLI.", "Install matching beam and beam-agentd versions from the same bundle.", tunnel.VersionError(status))
		}
		return renderer.Result(status, func(w io.Writer) error {
			state := "not ready"
			if status.Ready {
				state = "ready"
			}
			daemonVersion := status.DaemonVersion
			if daemonVersion == "" {
				daemonVersion = status.AgentVersion
			}
			_, err := fmt.Fprintf(w, "%s %s: %s (local API v%d)\n", version.AgentName(), daemonVersion, state, status.ProtocolVersion)
			return err
		})
	case "diagnostics":
		if len(args) != 1 {
			return usage("tunnel diagnostics does not accept arguments")
		}
		status, err := client.Status(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		if !tunnel.Compatible(status) {
			return cliError(ExitVersionIncompatible, "beam-agentd is incompatible with this CLI.", "Install matching beam and beam-agentd versions from the same bundle.", tunnel.VersionError(status))
		}
		metrics, err := client.Metrics(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		result := tunnel.Diagnostics{Status: status, Metrics: metrics}
		return renderer.Result(result, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "ready=%t endpoints=%d active=%d operations=%d running=%d failed=%d\n",
				status.Ready, metrics.EndpointsTotal, metrics.EndpointsActive, metrics.OperationsTotal, metrics.OperationsRunning, metrics.OperationsFailed)
			return err
		})
	case "studio":
		return a.tunnelStudio(ctx, args[1:], client, renderer)
	case "room":
		return a.roomCommand(ctx, args[1:], cfg, paths, renderer)
	case "expose":
		return tunnelExpose(ctx, args[1:], client, paths, renderer)
	case "receive":
		return tunnelReceive(ctx, args[1:], client, paths, renderer)
	case "list":
		if len(args) != 1 {
			return usage("tunnel list does not accept arguments")
		}
		return a.tunnelList(ctx, client, paths, renderer)
	case "show":
		if len(args) != 2 {
			return usage("tunnel show requires one tunnel name or ID")
		}
		return a.tunnelShow(ctx, args[1], client, paths, renderer)
	case "restart":
		if len(args) != 2 {
			return usage("tunnel restart requires one tunnel name or ID")
		}
		return a.tunnelRestart(ctx, args[1], client, paths, renderer)
	case "watch":
		return a.tunnelWatch(ctx, args[1:], client, paths, renderer)
	case "prune":
		if len(args) != 1 {
			return usage("tunnel prune does not accept arguments")
		}
		if err := ensureCompatible(ctx, client); err != nil {
			return err
		}
		result, err := client.Endpoints(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		removed, err := pruneTunnelNames(paths, result.Endpoints)
		if err != nil {
			return cliError(ExitConfig, "Could not prune local tunnel names.", "Check the Beam config directory.", err)
		}
		return renderer.Result(map[string]any{"removed_names": removed}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Pruned %d stale tunnel name(s).\n", len(removed))
			return err
		})
	case "endpoints":
		if len(args) != 1 {
			return usage("tunnel endpoints does not accept arguments")
		}
		if err := ensureCompatible(ctx, client); err != nil {
			return err
		}
		result, err := client.Endpoints(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(result, func(w io.Writer) error {
			rows := make([][]string, 0, len(result.Endpoints))
			for _, item := range result.Endpoints {
				rows = append(rows, []string{item.ID, item.Direction, item.Kind, item.Status, item.PublicURL})
			}
			return renderer.Table(w, output.Table{
				Headers: []string{"ENDPOINT ID", "DIRECTION", "KIND", "STATUS", "URL"},
				Rows:    rows,
				Style:   output.RowStyle(3),
			})
		})
	case "endpoint":
		if len(args) != 2 {
			return usage("tunnel endpoint requires one endpoint ID")
		}
		if err := ensureCompatible(ctx, client); err != nil {
			return err
		}
		result, err := client.Endpoint(ctx, args[1])
		if err != nil {
			return mapTunnelError(err)
		}
		return renderEndpoint(result, "", renderer)
	case "close":
		if len(args) != 2 {
			return usage("tunnel close requires one endpoint ID")
		}
		endpointID, resolveErr := resolveTunnel(paths, args[1])
		if resolveErr != nil {
			return cliError(ExitNotFound, "Tunnel was not found.", "Run `beam tunnel list`.", resolveErr)
		}
		if err := ensureCompatible(ctx, client); err != nil {
			return err
		}
		result, err := client.Close(ctx, endpointID)
		if err != nil {
			return mapTunnelError(err)
		}
		_ = forgetTunnel(paths, endpointID)
		return renderEndpoint(result, "", renderer)
	case "logs":
		return tunnelResourceLogs(ctx, args[1:], client, paths, renderer)
	case "upload", "download", "duplex":
		return tunnelTransferOperation(ctx, args[0], args[1:], client, renderer)
	case "bridge":
		return tunnelBridgeOperation(ctx, args[1:], client, renderer)
	case "identity":
		return tunnelIdentityOperation(ctx, args[1:], client, renderer)
	case "operations":
		return tunnelOperations(ctx, args[1:], client, renderer)
	case "operation":
		return tunnelOperation(ctx, args[1:], client, renderer)
	case "cancel":
		return tunnelCancelOperation(ctx, args[1:], client, renderer)
	default:
		return usage(fmt.Sprintf("unknown tunnel command %q", args[0]))
	}
}

func tunnelCommandUsesDaemon(command string) bool {
	switch command {
	case "status", "diagnostics", "studio", "expose", "receive", "list", "show", "restart", "watch", "prune",
		"endpoints", "endpoint", "close", "logs", "upload", "download", "duplex", "bridge", "identity",
		"operations", "operation", "cancel":
		return true
	default:
		return false
	}
}

func runRoomCommand(ctx context.Context, args []string, socketPath, cliVersion string, renderer output.Renderer) error {
	roomArgs := []string{"tunnel", "room"}
	roomArgs = append(roomArgs, args...)
	switch renderer.Mode {
	case output.JSON:
		roomArgs = append(roomArgs, "--json")
	case output.Quiet:
		roomArgs = append(roomArgs, "--quiet")
	}
	client := tunnel.NewLocalClient(socketPath).
		WithCLIVersion(cliVersion).
		WithProtocolRange(roomcli.MinimumLocalAPIVersion, tunnel.ProtocolVersion)
	var roomErr bytes.Buffer
	runner := roomcli.Runner{Client: client, In: os.Stdin, Out: renderer.Out, Err: &roomErr, Styled: renderer.OutInteractive}
	if code := runner.Run(ctx, roomArgs); code != roomcli.ExitOK {
		message := strings.TrimSpace(roomErr.String())
		message = strings.TrimPrefix(message, "beam room: ")
		message = strings.ReplaceAll(message, "beam-agent is", "beam-agentd is")
		if message == "" {
			message = "Room command failed."
		}
		if len(args) > 0 && args[0] == "join" {
			if invitationErr := specificRoomInvitationError(message, nil); invitationErr != nil {
				return invitationErr
			}
		}
		exit, hint := roomExitContract(code)
		return cliError(exit, message, hint, nil)
	}
	if roomErr.Len() > 0 && renderer.Mode == output.Human {
		_, _ = io.Copy(renderer.Err, &roomErr)
	}
	return nil
}

func roomExitContract(code int) (int, string) {
	switch code {
	case roomcli.ExitUsage:
		return ExitUsage, `Run "beam help room" for usage.`
	case roomcli.ExitDaemon:
		return ExitDaemonUnavailable, "Retry the command; Beam starts the agent automatically when a coordinator is configured."
	case roomcli.ExitCompatibility:
		return ExitVersionIncompatible, "Install matching beam and beam-agentd versions from the same bundle."
	case roomcli.ExitNotFound:
		return ExitNotFound, "Check the Room, channel, member, or publication ID."
	case roomcli.ExitConflict:
		return ExitConflict, "Refresh the Room state and try again."
	case roomcli.ExitDenied:
		return ExitAuth, "Check the active organization and Room permissions."
	default:
		return ExitOperationFailed, "Inspect `beam agent logs` and try again."
	}
}

func tunnelStart(ctx context.Context, args []string, cfg config.Config, cliVersion string, renderer output.Renderer) error {
	valueFlags := map[string]bool{
		"--coordinator": true, "--coordinators": true, "--coordinator-region": true,
		"--relay-id": true, "--transport": true, "--agent-id": true,
	}
	boolFlags := map[string]bool{"--discover-coordinators": true}
	if err := validateOptions(args, valueFlags, boolFlags); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, boolFlags)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usage("tunnel start does not accept positional arguments")
	}
	coordinator, _, err := flag(args, "--coordinator")
	if err != nil {
		return err
	}
	coordinators, _, err := flag(args, "--coordinators")
	if err != nil {
		return err
	}
	coordinatorRegion, _, err := flag(args, "--coordinator-region")
	if err != nil {
		return err
	}
	relayID, _, err := flag(args, "--relay-id")
	if err != nil {
		return err
	}
	transport, _, err := flag(args, "--transport")
	if err != nil {
		return err
	}
	if transport != "" && transport != "auto" && transport != "quic" && transport != "v1" && transport != "v0" {
		return usage("--transport must be auto, quic, v1, or v0")
	}
	agentID, _, err := flag(args, "--agent-id")
	if err != nil {
		return err
	}
	result, err := tunnel.StartDaemon(ctx, tunnel.StartOptions{
		SocketPath:           cfg.AgentSocket,
		CoordinatorURL:       coordinator,
		CoordinatorURLs:      coordinators,
		DiscoverCoordinators: has(args, "--discover-coordinators"), CoordinatorRegion: coordinatorRegion,
		RelayID: relayID, Transport: transport, AgentID: agentID,
		AgentBinary: os.Getenv("BEAM_TUNNEL_AGENT_BINARY"),
		CLIVersion:  cliVersion,
		LogPath:     os.Getenv("BEAM_AGENT_LOG"),
	})
	if err != nil {
		var tunnelErr *tunnel.Error
		if errors.As(err, &tunnelErr) && tunnelErr.Kind == tunnel.ErrVersion {
			return mapTunnelError(err)
		}
		return cliError(ExitDaemonUnavailable, "Could not start beam-agentd.", "Install the Beam bundle or set BEAM_AGENTD_BINARY. Inspect BEAM_AGENT_LOG if startup still fails.", err)
	}
	return renderer.Result(result, func(w io.Writer) error {
		if result.AlreadyRunning {
			_, err := fmt.Fprintf(w, "%s already running (local API v%d)\n", version.AgentName(), result.Status.ProtocolVersion)
			return err
		}
		_, err := fmt.Fprintf(w, "%s started (pid %d, local API v%d)\n", version.AgentName(), result.PID, result.Status.ProtocolVersion)
		return err
	})
}

func tunnelExpose(ctx context.Context, args []string, client tunnel.Client, paths config.Paths, renderer output.Renderer) error {
	if err := validateTunnelEndpointOptions(args); err != nil {
		return err
	}
	positionals, err := tunnelEndpointPositionals(args)
	if err != nil {
		return err
	}
	if len(positionals) != 2 {
		return usage("tunnel expose requires a kind and target")
	}
	kind, target := strings.ToLower(positionals[0]), positionals[1]
	switch kind {
	case "file":
		if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() {
			return usage("exposed file must exist and be a regular file")
		}
		absolute, err := filepath.Abs(target)
		if err != nil {
			return cliError(ExitOperationFailed, "Could not resolve the exposed file path.", "Use an absolute path and try again.", err)
		}
		target = absolute
	case "http", "stream", "webrtc", "tcp":
		if strings.TrimSpace(target) == "" {
			return usage("exposure target must not be empty")
		}
		if kind == "http" || kind == "stream" || kind == "webrtc" {
			if port, err := strconv.Atoi(target); err == nil && (port < 1 || port > 65535) {
				return usage("exposure port must be between 1 and 65535")
			}
		}
	default:
		return usage("exposure kind must be file, http, stream, webrtc, or tcp")
	}
	options, err := parseTunnelPublicOptions(args, kind != "tcp")
	if err != nil {
		return err
	}
	if kind == "tcp" && options.Public {
		return usage("tcp does not support --public")
	}
	if options.ObjectStorage && kind != "file" {
		return usage("--object-storage supports only file exposures and receive destinations")
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	result, err := withTunnelCreationProgress(renderer, tunnelProgressOptions{
		Public: options.Public, ObjectStorage: options.ObjectStorage,
	}, func() (tunnel.Endpoint, error) {
		return client.CreateExposure(ctx, tunnel.ExposureRequest{
			Kind: kind, Target: target, Public: options.Public,
			PublicEndpointKey: options.PublicEndpointKey, Standbys: options.Standbys,
			RelayID: options.RelayID, ShutdownGrace: options.ShutdownGrace, ObjectStorage: options.ObjectStorage,
		})
	})
	if err != nil {
		return mapTunnelError(err)
	}
	if err := rememberTunnel(paths, options.Name, result.ID); err != nil {
		return cliError(ExitConfig, "Tunnel was created but its local name could not be saved.", "Use the endpoint ID shown by `beam tunnel list`.", err)
	}
	if err := applyEndpointConveniences(result, options.Open, options.Copy); err != nil {
		renderer.Progress("Warning: " + err.Error())
	}
	return renderEndpoint(result, options.ObjectStorageConfigPath, renderer)
}

func tunnelReceive(ctx context.Context, args []string, client tunnel.Client, paths config.Paths, renderer output.Renderer) error {
	if err := validateTunnelEndpointOptions(args); err != nil {
		return err
	}
	positionals, err := tunnelEndpointPositionals(args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("tunnel receive requires one directory")
	}
	info, err := os.Stat(positionals[0])
	if err != nil || !info.IsDir() {
		return usage("receive destination must be an existing directory")
	}
	directory, err := filepath.Abs(positionals[0])
	if err != nil {
		return cliError(ExitOperationFailed, "Could not resolve the receive directory path.", "Use an absolute path and try again.", err)
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	options, err := parseTunnelPublicOptions(args, true)
	if err != nil {
		return err
	}
	result, err := withTunnelCreationProgress(renderer, tunnelProgressOptions{
		Public: options.Public, ObjectStorage: options.ObjectStorage,
	}, func() (tunnel.Endpoint, error) {
		return client.CreateDestination(ctx, tunnel.DestinationRequest{
			Directory: directory, Public: options.Public,
			PublicEndpointKey: options.PublicEndpointKey, Standbys: options.Standbys,
			RelayID: options.RelayID, ShutdownGrace: options.ShutdownGrace, ObjectStorage: options.ObjectStorage,
		})
	})
	if err != nil {
		return mapTunnelError(err)
	}
	if err := rememberTunnel(paths, options.Name, result.ID); err != nil {
		return cliError(ExitConfig, "Receive endpoint was created but its local name could not be saved.", "Use the endpoint ID shown by `beam tunnel list`.", err)
	}
	if err := applyEndpointConveniences(result, options.Open, options.Copy); err != nil {
		renderer.Progress("Warning: " + err.Error())
	}
	return renderEndpoint(result, options.ObjectStorageConfigPath, renderer)
}

type tunnelPublicOptions struct {
	Public                  bool
	PublicEndpointKey       string
	Standbys                int
	RelayID                 string
	ShutdownGrace           string
	ObjectStorage           bool
	ObjectStorageConfigPath string
	Name                    string
	Open                    bool
	Copy                    bool
}

func validateTunnelEndpointOptions(args []string) error {
	return validateOptions(args, map[string]bool{
		"--public-endpoint-key": true, "--standbys": true, "--relay-id": true, "--shutdown-grace": true,
		"--object-storage-config": true, "--name": true,
	}, map[string]bool{"--public": true, "--no-public": true, "--object-storage": true, "--open": true, "--copy": true, "--wait": true})
}

func tunnelEndpointPositionals(args []string) ([]string, error) {
	var result []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			result = append(result, arg)
			continue
		}
		name := arg
		if position := strings.IndexByte(arg, '='); position >= 0 {
			name = arg[:position]
		}
		if name == "--public" || name == "--no-public" || name == "--object-storage" || name == "--open" || name == "--copy" || name == "--wait" {
			continue
		}
		if !strings.Contains(arg, "=") {
			index++
		}
	}
	return result, nil
}

func parseTunnelPublicOptions(args []string, defaultPublic bool) (tunnelPublicOptions, error) {
	if has(args, "--public") && has(args, "--no-public") {
		return tunnelPublicOptions{}, usage("--public and --no-public cannot be used together")
	}
	options := tunnelPublicOptions{Public: defaultPublic, Standbys: 1, ShutdownGrace: "5s"}
	if has(args, "--public") {
		options.Public = true
	}
	if has(args, "--no-public") {
		options.Public = false
	}
	options.ObjectStorage = has(args, "--object-storage")
	options.Open = has(args, "--open")
	options.Copy = has(args, "--copy")
	var err error
	if options.Name, _, err = flag(args, "--name"); err != nil {
		return tunnelPublicOptions{}, err
	}
	if options.PublicEndpointKey, _, err = flag(args, "--public-endpoint-key"); err != nil {
		return tunnelPublicOptions{}, err
	}
	if options.RelayID, _, err = flag(args, "--relay-id"); err != nil {
		return tunnelPublicOptions{}, err
	}
	if options.ObjectStorageConfigPath, _, err = flag(args, "--object-storage-config"); err != nil {
		return tunnelPublicOptions{}, err
	}
	if options.ObjectStorageConfigPath != "" && !options.ObjectStorage {
		return tunnelPublicOptions{}, usage("--object-storage-config requires --object-storage")
	}
	if options.PublicEndpointKey == "" && options.Name != "" {
		options.PublicEndpointKey = options.Name
	}
	if raw, found, flagErr := flag(args, "--standbys"); flagErr != nil {
		return tunnelPublicOptions{}, flagErr
	} else if found {
		options.Standbys, err = strconv.Atoi(raw)
		if err != nil || options.Standbys < 0 || options.Standbys > 3 {
			return tunnelPublicOptions{}, usage("--standbys must be between 0 and 3")
		}
	}
	if raw, found, flagErr := flag(args, "--shutdown-grace"); flagErr != nil {
		return tunnelPublicOptions{}, flagErr
	} else if found {
		grace, parseErr := time.ParseDuration(raw)
		if parseErr != nil || grace < 0 || grace > time.Minute {
			return tunnelPublicOptions{}, usage("--shutdown-grace must be between 0 and 1m")
		}
		options.ShutdownGrace = grace.String()
	}
	if !options.Public {
		if options.PublicEndpointKey != "" || hasFlag(args, "--standbys") || hasFlag(args, "--shutdown-grace") || options.ObjectStorage {
			return tunnelPublicOptions{}, usage("public endpoint options require --public")
		}
		options.Standbys = 0
		options.ShutdownGrace = ""
	}
	return options, nil
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func tunnelLogs(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if err := validateOptions(args, nil, map[string]bool{"--follow": true}); err != nil {
		return err
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	follow := has(args, "--follow")
	var events []tunnel.LogEvent
	err := client.Logs(ctx, follow, func(event tunnel.LogEvent) error {
		if !follow {
			events = append(events, event)
			return nil
		}
		switch renderer.Mode {
		case output.Quiet:
			return nil
		case output.JSON:
			return json.NewEncoder(renderer.Out).Encode(event)
		default:
			_, err := fmt.Fprintf(renderer.Out, "%s\t%s\t%s\t%s\n", event.Time.Format("15:04:05"), event.Level, event.EndpointID, event.Message)
			return err
		}
	})
	if err != nil {
		return mapTunnelError(err)
	}
	if follow {
		return nil
	}
	return renderer.Result(map[string]any{"events": events}, func(w io.Writer) error {
		for _, event := range events {
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", event.Time.Format("15:04:05"), event.Level, event.EndpointID, event.Message); err != nil {
				return err
			}
		}
		return nil
	})
}

var operationValueFlags = map[string]bool{
	"--filename": true, "--session": true, "--workload-id": true, "--plan-version": true,
	"--manifest": true, "--receipts-out": true, "--upload-id": true,
	"--identity-key": true, "--agent-id": true, "--listen": true, "--target": true,
	"--min-relays": true, "--standbys": true, "--concurrency": true, "--part-size": true,
	"--transport": true,
}

func tunnelTransferOperation(ctx context.Context, operationType string, args []string, client tunnel.Client, renderer output.Renderer) error {
	if err := validateOptions(args, operationValueFlags, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("tunnel " + operationType + " requires one file path")
	}
	file, err := filepath.Abs(positionals[0])
	if err != nil {
		return usage("operation file path is invalid")
	}
	if operationType == "upload" {
		info, statErr := os.Stat(file)
		if statErr != nil || !info.Mode().IsRegular() {
			return usage("uploaded file must exist and be a regular file")
		}
	}
	request := tunnel.OperationRequest{Type: operationType, File: file}
	if err := populateTransferOptions(args, &request); err != nil {
		return err
	}
	if (operationType == "download" || operationType == "upload") && request.Session == "" && request.WorkloadID == "" {
		return usage("tunnel " + operationType + " requires --session or --workload-id")
	}
	return createTunnelOperation(ctx, client, request, renderer)
}

func tunnelBridgeOperation(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) > 0 && args[0] == "sign" {
		return tunnelBridgeSignOperation(ctx, args[1:], client, renderer)
	}
	if err := validateOptions(args, operationValueFlags, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("tunnel bridge requires one participant lease file")
	}
	lease, err := existingAbsoluteFile(positionals[0])
	if err != nil {
		return usage("bridge participant lease must be an existing file")
	}
	identityKey, err := requiredFlag(args, "--identity-key")
	if err != nil {
		return err
	}
	identityKey, err = existingAbsoluteFile(identityKey)
	if err != nil {
		return usage("bridge identity key must be an existing file")
	}
	request := tunnel.OperationRequest{Type: "bridge", BridgeLease: lease, IdentityKey: identityKey}
	request.Listen, _, err = flag(args, "--listen")
	if err != nil {
		return err
	}
	request.Target, _, err = flag(args, "--target")
	if err != nil {
		return err
	}
	request.Transport, _, err = flag(args, "--transport")
	if err != nil {
		return err
	}
	return createTunnelOperation(ctx, client, request, renderer)
}

func tunnelBridgeSignOperation(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if err := validateOptions(args, operationValueFlags, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil || len(positionals) != 1 {
		return usage("tunnel bridge sign requires one bridge intent file")
	}
	intent, err := existingAbsoluteFile(positionals[0])
	if err != nil {
		return usage("bridge intent must be an existing file")
	}
	key, err := requiredFlag(args, "--identity-key")
	if err != nil {
		return err
	}
	key, err = existingAbsoluteFile(key)
	if err != nil {
		return usage("bridge identity key must be an existing file")
	}
	agentID, err := requiredFlag(args, "--agent-id")
	if err != nil {
		return err
	}
	return createTunnelOperation(ctx, client, tunnel.OperationRequest{
		Type: "bridge_sign", File: intent, IdentityKey: key, AgentID: agentID,
	}, renderer)
}

func tunnelIdentityOperation(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) == 0 || args[0] != "generate" {
		return usage("tunnel identity requires the generate command")
	}
	args = args[1:]
	if err := validateOptions(args, operationValueFlags, nil); err != nil {
		return err
	}
	if positionals, err := positional(args); err != nil || len(positionals) != 0 {
		return usage("tunnel identity generate does not accept positional arguments")
	}
	key, err := requiredFlag(args, "--identity-key")
	if err != nil {
		return err
	}
	key, err = filepath.Abs(key)
	if err != nil {
		return usage("identity key path is invalid")
	}
	agentID, err := requiredFlag(args, "--agent-id")
	if err != nil {
		return err
	}
	return createTunnelOperation(ctx, client, tunnel.OperationRequest{
		Type: "identity_generate", IdentityKey: key, AgentID: agentID,
	}, renderer)
}

func tunnelOperations(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) != 0 {
		return usage("tunnel operations does not accept arguments")
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	result, err := client.Operations(ctx)
	if err != nil {
		return mapTunnelError(err)
	}
	return renderer.Result(result, func(w io.Writer) error {
		rows := make([][]string, 0, len(result.Operations))
		for _, operation := range result.Operations {
			rows = append(rows, []string{operation.ID, operation.Type, operation.Status})
		}
		return renderer.Table(w, output.Table{
			Headers: []string{"OPERATION ID", "TYPE", "STATUS"},
			Rows:    rows,
			Style:   output.RowStyle(2),
		})
	})
}

func tunnelOperation(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("tunnel operation requires one operation ID")
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	operation, err := client.Operation(ctx, args[0])
	if err != nil {
		return mapTunnelError(err)
	}
	return renderOperation(operation, renderer)
}

func tunnelCancelOperation(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("tunnel cancel requires one operation ID")
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	operation, err := client.CancelOperation(ctx, args[0])
	if err != nil {
		return mapTunnelError(err)
	}
	return renderOperation(operation, renderer)
}

func createTunnelOperation(ctx context.Context, client tunnel.Client, request tunnel.OperationRequest, renderer output.Renderer) error {
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	operation, err := client.CreateOperation(ctx, request)
	if err != nil {
		return mapTunnelError(err)
	}
	return renderOperation(operation, renderer)
}

func renderOperation(operation tunnel.Operation, renderer output.Renderer) error {
	return renderer.Result(operation, func(w io.Writer) error {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", operation.ID, operation.Type, operation.Status); err != nil {
			return err
		}
		if value, ok := operation.Result["output"].(string); ok && strings.TrimSpace(value) != "" {
			if _, err := fmt.Fprintln(w, value); err != nil {
				return err
			}
		}
		if operation.Error != nil && operation.Error.Message != "" {
			_, err := fmt.Fprintf(w, "error: %s (%s)\n", operation.Error.Message, operation.Error.Code)
			return err
		}
		return nil
	})
}

func populateTransferOptions(args []string, request *tunnel.OperationRequest) error {
	var err error
	request.Filename, _, err = flag(args, "--filename")
	if err != nil {
		return err
	}
	request.Session, _, err = flag(args, "--session")
	if err != nil {
		return err
	}
	if request.Session != "" {
		request.Session, err = existingAbsoluteFile(request.Session)
		if err != nil {
			return usage("session must be an existing file")
		}
	}
	request.WorkloadID, _, err = flag(args, "--workload-id")
	if err != nil {
		return err
	}
	request.Manifest, _, err = flag(args, "--manifest")
	if err != nil {
		return err
	}
	if request.Manifest != "" {
		request.Manifest, err = existingAbsoluteFile(request.Manifest)
		if err != nil {
			return usage("manifest must be an existing file")
		}
	}
	request.ReceiptsOut, _, err = flag(args, "--receipts-out")
	if err != nil {
		return err
	}
	if request.ReceiptsOut != "" {
		request.ReceiptsOut, err = filepath.Abs(request.ReceiptsOut)
		if err != nil {
			return usage("receipts path is invalid")
		}
	}
	request.UploadID, _, err = flag(args, "--upload-id")
	if err != nil {
		return err
	}
	request.Transport, _, err = flag(args, "--transport")
	if err != nil {
		return err
	}
	if request.PlanVersion, err = int64Option(args, "--plan-version", 0); err != nil {
		return err
	}
	if request.MinRelays, err = intOption(args, "--min-relays", 1); err != nil {
		return err
	}
	if request.Standbys, err = intOption(args, "--standbys", 1); err != nil {
		return err
	}
	if request.Concurrency, err = intOption(args, "--concurrency", 4); err != nil {
		return err
	}
	if request.PartSize, err = int64Option(args, "--part-size", 0); err != nil {
		return err
	}
	return nil
}

func intOption(args []string, name string, fallback int) (int, error) {
	value, ok, err := flag(args, name)
	if err != nil || !ok {
		return fallback, err
	}
	parsed, parseErr := strconv.Atoi(value)
	if parseErr != nil || parsed < 0 {
		return 0, usage(name + " must be a non-negative integer")
	}
	return parsed, nil
}

func int64Option(args []string, name string, fallback int64) (int64, error) {
	value, ok, err := flag(args, name)
	if err != nil || !ok {
		return fallback, err
	}
	parsed, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || parsed < 0 {
		return 0, usage(name + " must be a non-negative integer")
	}
	return parsed, nil
}

func existingAbsoluteFile(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	return absolute, nil
}

func ensureCompatible(ctx context.Context, client tunnel.Client) error {
	status, err := client.Status(ctx)
	if err != nil {
		return mapTunnelError(err)
	}
	if !tunnel.Compatible(status) {
		return cliError(ExitVersionIncompatible, "beam-agentd is incompatible with this CLI.", "Install matching beam and beam-agentd versions from the same bundle.", tunnel.VersionError(status))
	}
	return nil
}

func mapTunnelError(err error) error {
	var commandErr *Error
	if errors.As(err, &commandErr) {
		return commandErr
	}
	var tunnelErr *tunnel.Error
	if !errors.As(err, &tunnelErr) {
		return cliError(ExitOperationFailed, "beam-agentd operation failed.", "Inspect beam tunnel logs and try again.", err)
	}
	switch tunnelErr.Kind {
	case tunnel.ErrUnavailable:
		return cliError(ExitDaemonUnavailable, "beam-agentd became unavailable.", "Retry the command; Beam starts the agent automatically when a coordinator is configured.", err)
	case tunnel.ErrVersion:
		return cliError(ExitVersionIncompatible, "beam-agentd is incompatible with this CLI.", "Install matching beam and beam-agentd versions from the same bundle.", err)
	case tunnel.ErrNotFound:
		return cliError(ExitNotFound, "The requested daemon resource was not found.", "Check the endpoint or operation ID.", err)
	case tunnel.ErrConflict:
		return cliError(ExitConflict, "beam-agentd rejected the operation because of its current state.", "Inspect the resource state and try again.", err)
	default:
		message := tunnelErr.Detail
		if strings.TrimSpace(message) == "" {
			message = "beam-agentd operation failed."
		}
		return cliError(ExitOperationFailed, message, "Inspect beam tunnel logs and try again.", err)
	}
}

func renderEndpoint(result tunnel.Endpoint, objectStorageConfigPath string, renderer output.Renderer) error {
	if objectStorageConfigPath != "" {
		if result.ObjectStorage == nil {
			return cliError(ExitOperationFailed, "beam-agentd did not return an object-storage config.", "Inspect beam tunnel logs and try again.", nil)
		}
		if err := writeObjectStorageConfig(objectStorageConfigPath, *result.ObjectStorage); err != nil {
			return cliError(ExitOperationFailed, "Could not write the object-storage config.", "Check the path and its permissions, then try again.", err)
		}
	}
	return renderer.Result(result, func(w io.Writer) error {
		if result.ObjectStorage != nil {
			if objectStorageConfigPath != "" {
				absolute, err := filepath.Abs(objectStorageConfigPath)
				if err != nil {
					absolute = objectStorageConfigPath
				}
				_, err = fmt.Fprintln(w, absolute)
				return err
			}
			encoded, err := json.MarshalIndent(result.ObjectStorage, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(w, string(encoded))
			return err
		}
		if result.PublicURL != "" {
			_, err := fmt.Fprintln(w, renderer.HighlightURL(result.PublicURL))
			return err
		}
		_, err := fmt.Fprintf(w, "Endpoint %s: %s\n", result.ID, result.Status)
		return err
	})
}

func writeObjectStorageConfig(path string, config tunnel.ObjectStorageConfig) error {
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
