package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/browser"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func (a *App) share(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("share requires a file, port, or URL")
	}
	target := args[0]
	kind := "http"
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		kind = "file"
	} else if port := strings.TrimPrefix(target, ":"); port != target {
		if _, err := strconv.Atoi(port); err != nil {
			return usage("share port is invalid")
		}
		target = port
	} else if _, err := strconv.Atoi(target); err != nil && !strings.Contains(target, "://") {
		return usage("share target must be an existing file, port, or URL")
	}
	rewritten := []string{kind, target}
	for _, arg := range args[1:] {
		switch arg {
		case "--private":
			rewritten = append(rewritten, "--no-public")
		case "--s3":
			rewritten = append(rewritten, "--object-storage")
		default:
			rewritten = append(rewritten, arg)
		}
	}
	return tunnelExpose(ctx, rewritten, a.autoStartingLocalClient(cfg), paths, renderer)
}

func (a *App) receive(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("receive requires one directory")
	}
	rewritten := []string{args[0]}
	for _, arg := range args[1:] {
		switch arg {
		case "--private":
			rewritten = append(rewritten, "--no-public")
		case "--s3":
			rewritten = append(rewritten, "--object-storage")
		default:
			rewritten = append(rewritten, arg)
		}
	}
	return tunnelReceive(ctx, rewritten, a.autoStartingLocalClient(cfg), paths, renderer)
}

func (a *App) unshare(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("unshare requires a tunnel name, ID, or --last")
	}
	id, err := resolveTunnel(paths, args[0])
	if err != nil {
		return cliError(ExitNotFound, "Tunnel was not found.", "Run `beam tunnel list`.", err)
	}
	client, err := a.readyLocalClient(ctx, cfg)
	if err != nil {
		return err
	}
	endpoint, err := client.Close(ctx, id)
	if err != nil {
		return mapTunnelError(err)
	}
	_ = forgetTunnel(paths, id)
	return renderEndpoint(endpoint, "", renderer)
}

func (a *App) readyLocalClient(ctx context.Context, cfg config.Config) (*tunnel.LocalClient, error) {
	client := tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version)
	if status, err := client.Status(ctx); err == nil {
		if !tunnel.Compatible(status) {
			return nil, cliError(ExitVersionIncompatible, "beam-agentd is incompatible with this CLI.", "Run `beam update`.", tunnel.VersionError(status))
		}
		return client, nil
	}
	if _, err := a.startAgentDaemon(ctx, cfg, cfg.CoordinatorURL); err != nil {
		return nil, err
	}
	return client, nil
}

func (a *App) tunnelList(ctx context.Context, client tunnel.Client, paths config.Paths, renderer output.Renderer) error {
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	result, err := client.Endpoints(ctx)
	if err != nil {
		return mapTunnelError(err)
	}
	names := tunnelNames(paths)
	view := make([]map[string]any, 0, len(result.Endpoints))
	for _, endpoint := range result.Endpoints {
		view = append(view, map[string]any{"name": names[endpoint.ID], "endpoint": endpoint})
	}
	return renderer.Result(map[string]any{"tunnels": view}, func(w io.Writer) error {
		rows := make([][]string, 0, len(result.Endpoints))
		for _, endpoint := range result.Endpoints {
			rows = append(rows, []string{firstNonEmpty(names[endpoint.ID], "-"), endpoint.ID, endpoint.Direction, endpoint.Kind, endpoint.Status, endpoint.PublicURL})
		}
		return renderer.Table(w, output.Table{
			Headers: []string{"NAME", "ENDPOINT ID", "DIRECTION", "KIND", "STATUS", "URL"},
			Rows:    rows,
			Style:   output.RowStyle(4),
		})
	})
}

func (a *App) tunnelShow(ctx context.Context, requested string, client tunnel.Client, paths config.Paths, renderer output.Renderer) error {
	id, err := resolveTunnel(paths, requested)
	if err != nil {
		return cliError(ExitNotFound, "Tunnel was not found.", "Run `beam tunnel list`.", err)
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	endpoint, err := client.Endpoint(ctx, id)
	if err != nil {
		return mapTunnelError(err)
	}
	return renderEndpoint(endpoint, "", renderer)
}

func (a *App) tunnelRestart(ctx context.Context, requested string, client tunnel.Client, paths config.Paths, renderer output.Renderer) error {
	id, err := resolveTunnel(paths, requested)
	if err != nil {
		return err
	}
	endpoint, err := client.Endpoint(ctx, id)
	if err != nil {
		return mapTunnelError(err)
	}
	if _, err := client.Close(ctx, id); err != nil {
		return mapTunnelError(err)
	}
	var restarted tunnel.Endpoint
	if endpoint.Direction == "destination" {
		restarted, err = client.CreateDestination(ctx, tunnel.DestinationRequest{Directory: endpoint.Target, Public: endpoint.Public, PublicEndpointKey: endpoint.PublicKey, Standbys: boolInt(endpoint.Public)})
	} else {
		restarted, err = client.CreateExposure(ctx, tunnel.ExposureRequest{Kind: endpoint.Kind, Target: endpoint.Target, Public: endpoint.Public, PublicEndpointKey: endpoint.PublicKey, Standbys: boolInt(endpoint.Public)})
	}
	if err != nil {
		return mapTunnelError(err)
	}
	name := tunnelNames(paths)[id]
	_ = forgetTunnel(paths, id)
	if err := rememberTunnel(paths, name, restarted.ID); err != nil {
		return err
	}
	return renderEndpoint(restarted, "", renderer)
}

func (a *App) tunnelWatch(ctx context.Context, args []string, client tunnel.Client, paths config.Paths, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("tunnel watch requires one tunnel name or ID")
	}
	id, err := resolveTunnel(paths, args[0])
	if err != nil {
		return err
	}
	previous := ""
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		endpoint, err := client.Endpoint(ctx, id)
		if err != nil {
			return mapTunnelError(err)
		}
		encoded, _ := json.Marshal(endpoint)
		if string(encoded) != previous {
			if renderer.Mode == output.JSON {
				if err := json.NewEncoder(renderer.Out).Encode(endpoint); err != nil {
					return err
				}
			} else if renderer.Mode == output.Human {
				_, _ = fmt.Fprintf(renderer.Out, "%s\t%s\t%s\n", endpoint.ID, endpoint.Status, endpoint.PublicURL)
			}
			previous = string(encoded)
		}
		if endpoint.Status == "closed" || endpoint.Status == "failed" || endpoint.Status == "expired" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func tunnelResourceLogs(ctx context.Context, args []string, client tunnel.Client, paths config.Paths, renderer output.Renderer) error {
	follow := has(args, "--follow")
	clean := withoutArgs(args, "--follow")
	if len(clean) == 0 {
		return tunnelLogs(ctx, args, client, renderer)
	}
	if len(clean) != 1 {
		return usage("tunnel logs accepts at most one tunnel name or ID")
	}
	id, err := resolveTunnel(paths, clean[0])
	if err != nil {
		return cliError(ExitNotFound, "Tunnel was not found.", "Run `beam tunnel list`.", err)
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	return client.Logs(ctx, follow, func(event tunnel.LogEvent) error {
		if event.EndpointID != id {
			return nil
		}
		if renderer.Mode == output.JSON {
			return json.NewEncoder(renderer.Out).Encode(event)
		}
		if renderer.Mode == output.Human {
			_, err := fmt.Fprintf(renderer.Out, "%s\t%s\t%s\n", event.Time.Format("15:04:05"), event.Level, event.Message)
			return err
		}
		return nil
	})
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

type resourceState struct {
	Tunnels    map[string]string `json:"tunnels,omitempty"`
	LastTunnel string            `json:"last_tunnel,omitempty"`
}

func resourceStatePath(paths config.Paths) string {
	if paths.Dir == "" {
		return ""
	}
	name := "resources.json"
	if paths.ContextName != "" {
		name = "resources-" + paths.ContextName + ".json"
	}
	return filepath.Join(paths.Dir, name)
}

func loadResourceState(paths config.Paths) (resourceState, error) {
	state := resourceState{Tunnels: map[string]string{}}
	path := resourceStatePath(paths)
	if path == "" {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Tunnels == nil {
		state.Tunnels = map[string]string{}
	}
	return state, nil
}

func saveResourceState(paths config.Paths, state resourceState) error {
	path := resourceStatePath(paths)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	// Windows cannot atomically rename a temporary file over an existing file.
	// Match config.Save's safe owner-local overwrite behavior there.
	if runtime.GOOS == "windows" {
		return os.WriteFile(path, encoded, 0o600)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".resources-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func rememberTunnel(paths config.Paths, name, id string) error {
	if paths.Dir == "" || id == "" {
		return nil
	}
	state, err := loadResourceState(paths)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) != "" {
		if !config.ValidContextName(name) {
			return fmt.Errorf("invalid tunnel name %q", name)
		}
		state.Tunnels[name] = id
	}
	state.LastTunnel = id
	return saveResourceState(paths, state)
}

func resolveTunnel(paths config.Paths, requested string) (string, error) {
	state, err := loadResourceState(paths)
	if err != nil {
		return "", err
	}
	if requested == "--last" || requested == "last" {
		if state.LastTunnel == "" {
			return "", fmt.Errorf("no previous tunnel is recorded")
		}
		return state.LastTunnel, nil
	}
	if id := state.Tunnels[requested]; id != "" {
		return id, nil
	}
	return requested, nil
}

func tunnelNames(paths config.Paths) map[string]string {
	state, err := loadResourceState(paths)
	if err != nil {
		return nil
	}
	result := make(map[string]string, len(state.Tunnels))
	for name, id := range state.Tunnels {
		result[id] = name
	}
	return result
}

func forgetTunnel(paths config.Paths, id string) error {
	state, err := loadResourceState(paths)
	if err != nil {
		return err
	}
	for name, candidate := range state.Tunnels {
		if candidate == id {
			delete(state.Tunnels, name)
		}
	}
	if state.LastTunnel == id {
		state.LastTunnel = ""
	}
	return saveResourceState(paths, state)
}

func pruneTunnelNames(paths config.Paths, endpoints []tunnel.Endpoint) ([]string, error) {
	state, err := loadResourceState(paths)
	if err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	for _, endpoint := range endpoints {
		existing[endpoint.ID] = true
	}
	var removed []string
	for name, id := range state.Tunnels {
		if !existing[id] {
			removed = append(removed, name)
			delete(state.Tunnels, name)
		}
	}
	if state.LastTunnel != "" && !existing[state.LastTunnel] {
		state.LastTunnel = ""
	}
	sort.Strings(removed)
	return removed, saveResourceState(paths, state)
}

func applyEndpointConveniences(endpoint tunnel.Endpoint, open, copy bool) error {
	if !open && !copy {
		return nil
	}
	if endpoint.PublicURL == "" {
		return errors.New("the endpoint has no public URL")
	}
	var problems []error
	if open {
		problems = append(problems, browser.Open(endpoint.PublicURL))
	}
	if copy {
		problems = append(problems, copyText(endpoint.PublicURL))
	}
	return errors.Join(problems...)
}

func copyText(value string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("pbcopy")
	case "windows":
		command = exec.Command("clip")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			command = exec.Command("wl-copy")
		} else {
			command = exec.Command("xclip", "-selection", "clipboard")
		}
	}
	command.Stdin = strings.NewReader(value)
	return command.Run()
}
