package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/roommanager"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func (a *App) roomCommand(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 {
		return a.roomRun(ctx, args, cfg.AgentSocket, a.version.Version, renderer)
	}
	if args[0] == "join" {
		return a.roomJoin(ctx, args[1:], cfg, paths, renderer)
	}
	if isLegacyRoomCommand(args[0]) {
		if args[0] == "create" {
			requested, remaining, flagErr := takeOrganizationFlag(args)
			if flagErr != nil {
				return flagErr
			}
			if verifyErr := a.verifyRoomOrganization(ctx, requested, remaining, cfg, paths); verifyErr != nil {
				return verifyErr
			}
			args = remaining
		}
		return a.runRoomReady(ctx, args, cfg, renderer)
	}
	if len(args) < 2 {
		return usage("room requires an action after the Room name or ID")
	}
	roomID, action, rest := args[0], args[1], args[2:]
	client, readyErr := a.readyLocalClient(ctx, cfg)
	if readyErr != nil {
		return readyErr
	}
	switch action {
	case "show":
		return a.runRoomReady(ctx, []string{"inspect", roomID}, cfg, renderer)
	case "refresh", "leave", "close":
		return a.runRoomReady(ctx, append([]string{action, roomID}, rest...), cfg, renderer)
	case "watch":
		return watchRoom(ctx, roomID, client, renderer)
	case "invite":
		normalized := append([]string{"invite", roomID}, rest...)
		if hasFlag(rest, "--out") {
			return a.roomInviteToFile(ctx, normalized, cfg, renderer)
		}
		return a.runRoomReady(ctx, normalized, cfg, renderer)
	case "invitation":
		return roomInvitationCommand(ctx, roomID, rest, client, renderer)
	case "member", "members":
		return a.roomMemberCommand(ctx, roomID, rest, client, renderer)
	case "role":
		return a.roomRoleCommand(ctx, roomID, rest, client, cfg, renderer)
	case "channel":
		if len(rest) >= 3 && rest[1] == "media" && rest[2] == "stop" {
			return roomMediaStop(ctx, roomID, rest[0], rest[3:], client, renderer)
		}
		normalized, err := normalizeScopedChannel(roomID, rest)
		if err != nil {
			return err
		}
		return a.runRoomReady(ctx, normalized, cfg, renderer)
	default:
		return usage(fmt.Sprintf("unknown room action %q", action))
	}
}

func (a *App) roomJoin(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	valueFlags := map[string]bool{
		"--invitation-token": true, "--invitation-file": true, "--lease-ttl": true,
		"--idempotency-key": true, "--coordinator": true,
	}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, nil)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("room join requires one Room ID")
	}
	roomID := positionals[0]
	coordinatorURL, coordinatorProvided, err := flag(args, "--coordinator")
	if err != nil {
		return err
	}
	if coordinatorURL == "" {
		coordinatorURL = cfg.CoordinatorURL
	}
	cleanArgs := removeRoomJoinOption(args, "--coordinator")
	invitationToken, leaseTTL, idempotencyKey, err := readRoomJoinBootstrapOptions(cleanArgs)
	if err != nil {
		return err
	}
	if idempotencyKey == "" {
		idempotencyKey = roomJoinIdempotencyKey(roomID, invitationToken)
	}
	normalizedJoinArgs := []string{
		"join", roomID,
		"--invitation-token", invitationToken,
		"--lease-ttl", strconv.Itoa(leaseTTL),
		"--idempotency-key", idempotencyKey,
	}
	client := tunnel.NewLocalClient(cfg.AgentSocket).WithCLIVersion(a.version.Version)
	status, statusErr := client.AgentConnection(ctx)
	if statusErr != nil {
		if coordinatorURL == "" {
			return cliError(ExitDaemonUnavailable, "beam-agentd is stopped and no coordinator is configured.", "Use `--coordinator <url>` or set BEAM_COORDINATOR_URL.", statusErr)
		}
		if _, err := a.startAgentDaemon(ctx, cfg, coordinatorURL); err != nil {
			return err
		}
		status, statusErr = client.AgentConnection(ctx)
	}
	if statusErr != nil {
		return mapTunnelError(statusErr)
	}
	if status.Registered {
		if coordinatorProvided && coordinatorURL != "" && status.CoordinatorURL != "" && !sameEndpoint(coordinatorURL, status.CoordinatorURL) {
			return typedCLIError(ExitConflict, errorKindAgentConflict, "Agent is registered with another coordinator.", "Run `beam agent disconnect` before joining through a different coordinator.", nil)
		}
		return a.roomRun(ctx, normalizedJoinArgs, cfg.AgentSocket, a.version.Version, renderer)
	}
	if coordinatorURL == "" {
		coordinatorURL = status.CoordinatorURL
	}
	if coordinatorURL == "" {
		return usage("--coordinator is required until a default coordinator is configured with BEAM_COORDINATOR_URL")
	}
	status, err = client.ConnectAgentWithRoomInvitation(ctx, tunnel.AgentRoomInvitationConnectionRequest{
		CoordinatorURL: coordinatorURL, RoomID: roomID, InvitationToken: invitationToken, LeaseTTLSeconds: leaseTTL,
	}, idempotencyKey)
	if err != nil {
		if roomInvitationAlreadyJoined(err) {
			if current, statusErr := client.AgentConnection(ctx); statusErr == nil && current.Registered {
				status = current
			} else {
				return mapRoomInvitationError(err)
			}
		} else {
			return mapRoomInvitationError(err)
		}
	}
	// Room runtimes are initialized from the daemon-owned credential at startup.
	// The coordinator membership is already committed, so the restarted daemon
	// adopts it through authenticated room discovery without replaying the code.
	if _, err := client.Shutdown(ctx); err != nil {
		return mapTunnelError(err)
	}
	if err := waitForAgentDaemonStop(ctx, client); err != nil {
		return err
	}
	if _, err := a.startAgentDaemon(ctx, cfg, coordinatorURL); err != nil {
		return err
	}
	if err := saveAgentSelection(paths, cfg, coordinatorURL, status.OrganizationID); err != nil {
		return err
	}
	if err := waitForJoinedRoom(ctx, client, roomID); err != nil {
		return err
	}
	return a.roomRun(ctx, []string{"inspect", roomID}, cfg.AgentSocket, a.version.Version, renderer)
}

func readRoomJoinBootstrapOptions(args []string) (string, int, string, error) {
	invitationToken, _, err := flag(args, "--invitation-token")
	if err != nil {
		return "", 0, "", err
	}
	invitationFile, _, err := flag(args, "--invitation-file")
	if err != nil {
		return "", 0, "", err
	}
	if invitationToken != "" && invitationFile != "" {
		return "", 0, "", usage("room join accepts only one of --invitation-token or --invitation-file")
	}
	if invitationFile != "" {
		info, statErr := os.Lstat(invitationFile)
		if statErr != nil {
			return "", 0, "", typedCLIError(ExitNotFound, errorKindInvitationInvalid, "The invitation file could not be found.", "Check the path and try again.", statErr)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256<<10 {
			return "", 0, "", typedCLIError(ExitUsage, errorKindInvitationInvalid, "The invitation must be a non-empty regular file within the token size limit.", "Choose the invitation token file created by the Room owner.", nil)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return "", 0, "", typedCLIError(ExitUsage, errorKindInvitationInvalid, "The invitation file is accessible by other users.", "Run `chmod 600 "+invitationFile+"` and try again.", nil)
		}
		payload, readErr := os.ReadFile(invitationFile)
		if readErr != nil {
			return "", 0, "", cliError(ExitOperationFailed, "Could not read the invitation file.", "Check the file path and permissions.", readErr)
		}
		invitationToken = strings.TrimSpace(string(payload))
	}
	if invitationToken == "" {
		return "", 0, "", typedCLIError(ExitUsage, errorKindInvitationInvalid, "Room join requires an invitation token or file.", "Use --invitation-file FILE or --invitation-token TOKEN.", nil)
	}
	leaseTTL := 60
	if raw, ok, flagErr := flag(args, "--lease-ttl"); flagErr != nil {
		return "", 0, "", flagErr
	} else if ok {
		leaseTTL, err = strconv.Atoi(raw)
		if err != nil {
			return "", 0, "", usage("--lease-ttl must be an integer")
		}
		if leaseTTL <= 0 {
			return "", 0, "", usage("--lease-ttl must be greater than zero")
		}
	}
	idempotencyKey, _, err := flag(args, "--idempotency-key")
	return invitationToken, leaseTTL, idempotencyKey, err
}

func roomJoinIdempotencyKey(roomID, invitationToken string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(roomID) + "\x00" + strings.TrimSpace(invitationToken)))
	return fmt.Sprintf("beam-room-join-%x", digest[:16])
}

func roomInvitationAlreadyJoined(err error) bool {
	var tunnelErr *tunnel.Error
	if !errors.As(err, &tunnelErr) {
		return false
	}
	value := strings.ToLower(tunnelErr.Code + " " + tunnelErr.Detail)
	return strings.Contains(value, "already_member") || strings.Contains(value, "already joined") || strings.Contains(value, "already_joined")
}

func mapRoomInvitationError(err error) error {
	var tunnelErr *tunnel.Error
	if !errors.As(err, &tunnelErr) {
		return mapTunnelError(err)
	}
	if specific := specificRoomInvitationError(tunnelErr.Code+" "+tunnelErr.Detail, err); specific != nil {
		return specific
	}
	if tunnelErr.Kind == tunnel.ErrNotFound {
		return typedCLIError(ExitNotFound, errorKindInvitationInvalid, "The Room invitation is invalid or no longer exists.", "Check the Room ID or ask a Room owner for a new invitation.", err)
	}
	return mapTunnelError(err)
}

func specificRoomInvitationError(description string, cause error) error {
	value := strings.ToLower(description)
	switch {
	case strings.Contains(value, "expired"):
		return typedCLIError(ExitAuth, errorKindInvitationExpired, "The Room invitation has expired.", "Ask a Room owner for a new invitation.", cause)
	case strings.Contains(value, "revoked"):
		return typedCLIError(ExitAuth, errorKindInvitationRevoked, "The Room invitation has been revoked.", "Ask a Room owner for a new invitation.", cause)
	case strings.Contains(value, "consumed"), strings.Contains(value, "already used"), strings.Contains(value, "max_uses"):
		return typedCLIError(ExitConflict, errorKindInvitationConsumed, "The Room invitation has already been consumed.", "Ask a Room owner for a new invitation or retry on the machine that already joined.", cause)
	case strings.Contains(value, "room_mismatch"), strings.Contains(value, "wrong room"), strings.Contains(value, "different room"):
		return typedCLIError(ExitConflict, errorKindInvitationRoomMismatch, "The invitation does not belong to this Room.", "Use the Room ID associated with the invitation.", cause)
	default:
		return nil
	}
}

func removeRoomJoinOption(args []string, name string) []string {
	clean := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == name {
			index++
			continue
		}
		if strings.HasPrefix(args[index], name+"=") {
			continue
		}
		clean = append(clean, args[index])
	}
	return clean
}

func waitForJoinedRoom(ctx context.Context, client *tunnel.LocalClient, roomID string) error {
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	path := "/v1/rooms/" + url.PathEscape(roomID)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return cliError(ExitOperationFailed, "Agent enrolled, but the joined room was not discovered locally.", "Run `beam room list` or restart the agent.", nil)
		case <-ticker.C:
			var room map[string]any
			if err := client.Do(ctx, http.MethodGet, path, "", nil, &room); err == nil {
				return nil
			}
		}
	}
}

func (a *App) runRoomReady(ctx context.Context, args []string, cfg config.Config, renderer output.Renderer) error {
	err := a.roomRun(ctx, args, cfg.AgentSocket, a.version.Version, renderer)
	var cliErr *Error
	if !errors.As(err, &cliErr) || cliErr.Code != ExitDaemonUnavailable {
		return err
	}
	if _, startErr := a.startAgentDaemon(ctx, cfg, cfg.CoordinatorURL); startErr != nil {
		return startErr
	}
	return a.roomRun(ctx, args, cfg.AgentSocket, a.version.Version, renderer)
}

func isLegacyRoomCommand(command string) bool {
	switch command {
	case "list", "inspect", "create", "join", "leave", "close", "refresh", "invite", "members", "role", "channel", "grant", "help", "--help", "-h":
		return true
	default:
		return false
	}
}

func normalizeScopedChannel(roomID string, args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, usage("room channel requires list, create, or a channel ID")
	}
	if args[0] == "list" || args[0] == "create" {
		return append([]string{"channel", args[0], roomID}, args[1:]...), nil
	}
	if len(args) < 2 {
		return nil, usage("room channel requires an action after the channel ID")
	}
	channelID, action, rest := args[0], args[1], args[2:]
	switch action {
	case "show":
		return []string{"channel", "inspect", roomID, channelID}, nil
	case "update", "close", "publish", "listen":
		return append([]string{"channel", action, roomID, channelID}, rest...), nil
	case "stream", "datagram":
		if len(rest) == 0 {
			return nil, usage("channel " + action + " requires publish, listen, or send")
		}
		return append([]string{"channel", action, roomID, rest[0], channelID}, rest[1:]...), nil
	case "object":
		if len(rest) == 0 {
			return nil, usage("channel object requires publish, list, show, watch, or cancel")
		}
		objectAction := rest[0]
		objectRest := rest[1:]
		if objectAction == "show" {
			objectAction = "status"
		}
		if objectAction == "watch" {
			objectAction = "status"
			objectRest = append(objectRest, "--follow")
		}
		return append([]string{"channel", "object", objectAction, roomID, channelID}, objectRest...), nil
	case "media":
		if len(rest) == 0 {
			return nil, usage("channel media requires publish, view, list, or stop")
		}
		return append([]string{"channel", "media", rest[0], roomID, channelID}, rest[1:]...), nil
	case "persistence":
		if len(rest) == 0 {
			return nil, usage("channel persistence requires status, retry, acknowledge, or purge")
		}
		return append([]string{"channel", "persistence", roomID, channelID, rest[0]}, rest[1:]...), nil
	case "grant":
		if len(rest) == 0 {
			return nil, usage("channel grant requires list, put, or revoke")
		}
		return append([]string{"grant", rest[0], roomID, channelID}, rest[1:]...), nil
	default:
		return nil, usage(fmt.Sprintf("unknown channel action %q", action))
	}
}

func (a *App) roomMemberCommand(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage("room member list does not accept arguments")
		}
		return renderRoomMembers(ctx, roomID, "", client, renderer)
	case "show":
		if len(args) != 2 {
			return usage("room member show requires one member ID")
		}
		return renderRoomMembers(ctx, roomID, args[1], client, renderer)
	case "watch":
		return watchRoomMembers(ctx, roomID, client, renderer)
	case "remove":
		if len(args) != 2 {
			return usage("room member remove requires one member ID")
		}
		var response map[string]any
		path := roomPathForCommand(roomID) + "/memberships/" + url.PathEscape(args[1])
		if err := client.Do(ctx, http.MethodDelete, path, "", map[string]any{}, &response); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(response, func(w io.Writer) error { return writeIndentedJSON(w, response) })
	default:
		return usage(fmt.Sprintf("unknown room member command %q", args[0]))
	}
}

func renderRoomMembers(ctx context.Context, roomID, selected string, client *tunnel.LocalClient, renderer output.Renderer) error {
	var response struct {
		Memberships []map[string]any `json:"memberships"`
	}
	if err := client.Do(ctx, http.MethodGet, roomPathForCommand(roomID)+"/memberships", "", nil, &response); err != nil {
		return mapTunnelError(err)
	}
	if selected != "" {
		for _, membership := range response.Memberships {
			if fmt.Sprint(membership["member_id"]) == selected {
				return renderer.Result(membership, func(w io.Writer) error { return writeIndentedJSON(w, membership) })
			}
		}
		return cliError(ExitNotFound, "Room member was not found.", "Run `beam room <room> member list`.", nil)
	}
	return renderer.Result(response, func(w io.Writer) error {
		rows := make([][]string, 0, len(response.Memberships))
		for _, membership := range response.Memberships {
			rows = append(rows, []string{
				fmt.Sprint(membership["member_id"]),
				fmt.Sprint(membership["state"]),
				fmt.Sprint(membership["presence"]),
				fmt.Sprint(membership["agent_id"]),
			})
		}
		return renderer.Table(w, output.Table{
			Headers: []string{"MEMBER ID", "STATE", "PRESENCE", "AGENT ID"},
			Rows:    rows,
			Style:   output.RowStyle(1),
		})
	})
}

func (a *App) roomRoleCommand(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, cfg config.Config, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	if args[0] == "show" {
		if len(args) != 2 {
			return usage("room role show requires one role ID")
		}
		var response struct {
			Roles []map[string]any `json:"roles"`
		}
		if err := client.Do(ctx, http.MethodGet, roomPathForCommand(roomID)+"/roles", "", nil, &response); err != nil {
			return mapTunnelError(err)
		}
		for _, role := range response.Roles {
			if fmt.Sprint(role["role_id"]) == args[1] {
				return renderer.Result(role, func(w io.Writer) error { return writeIndentedJSON(w, role) })
			}
		}
		return cliError(ExitNotFound, "Room role was not found.", "Run `beam room <room> role list`.", nil)
	}
	if args[0] == "delete" {
		if len(args) != 2 {
			return usage("room role delete requires one role ID")
		}
		var response map[string]any
		path := roomPathForCommand(roomID) + "/roles/" + url.PathEscape(args[1])
		if err := client.Do(ctx, http.MethodDelete, path, "", map[string]any{}, &response); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(response, func(w io.Writer) error { return writeIndentedJSON(w, response) })
	}
	normalized := append([]string{"role", args[0], roomID}, args[1:]...)
	return a.roomRun(ctx, normalized, cfg.AgentSocket, a.version.Version, renderer)
}

func roomInvitationCommand(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	base := roomPathForCommand(roomID) + "/invitations"
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage("room invitation list does not accept arguments")
		}
		var response struct {
			Invitations []map[string]any `json:"invitations"`
		}
		if err := client.Do(ctx, http.MethodGet, base, "", nil, &response); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(response, func(w io.Writer) error {
			rows := make([][]string, 0, len(response.Invitations))
			for _, invitation := range response.Invitations {
				rows = append(rows, []string{
					fmt.Sprint(invitation["invitation_id"]),
					fmt.Sprint(invitation["state"]),
					fmt.Sprintf("%v/%v", invitation["use_count"], invitation["max_uses"]),
					fmt.Sprint(invitation["expires_at"]),
				})
			}
			return renderer.Table(w, output.Table{
				Headers: []string{"INVITATION ID", "STATE", "USES", "EXPIRES"},
				Rows:    rows,
				Style:   output.RowStyle(1),
			})
		})
	case "show":
		if len(args) != 2 {
			return usage("room invitation show requires one invitation ID")
		}
		var response map[string]any
		if err := client.Do(ctx, http.MethodGet, base+"/"+url.PathEscape(args[1]), "", nil, &response); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(response, func(w io.Writer) error { return writeIndentedJSON(w, response) })
	case "revoke":
		if len(args) != 2 {
			return usage("room invitation revoke requires one invitation ID")
		}
		var response map[string]any
		if err := client.Do(ctx, http.MethodDelete, base+"/"+url.PathEscape(args[1]), "", map[string]any{}, &response); err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(response, func(w io.Writer) error { return writeIndentedJSON(w, response) })
	default:
		return usage(fmt.Sprintf("unknown room invitation command %q", args[0]))
	}
}

func roomMediaStop(ctx context.Context, roomID, channelID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("room channel media stop requires one workload ID")
	}
	var response map[string]any
	path := roomPathForCommand(roomID) + "/channels/" + url.PathEscape(channelID) + "/workloads/" + url.PathEscape(args[0]) + "/cancel"
	digest := sha256.Sum256([]byte(roomID + "\x00" + channelID + "\x00" + args[0]))
	idempotencyKey := fmt.Sprintf("beam-room-media-stop-%x", digest[:16])
	if err := client.Do(ctx, http.MethodPost, path, idempotencyKey, map[string]any{}, &response); err != nil {
		return mapTunnelError(err)
	}
	return renderer.Result(response, func(w io.Writer) error { return writeIndentedJSON(w, response) })
}

func watchRoom(ctx context.Context, roomID string, client *tunnel.LocalClient, renderer output.Renderer) error {
	return pollRoomResource(ctx, roomPathForCommand(roomID), client, renderer)
}

func watchRoomMembers(ctx context.Context, roomID string, client *tunnel.LocalClient, renderer output.Renderer) error {
	return pollRoomResource(ctx, roomPathForCommand(roomID)+"/memberships", client, renderer)
}

func pollRoomResource(ctx context.Context, path string, client *tunnel.LocalClient, renderer output.Renderer) error {
	previous := ""
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var response map[string]any
		if err := client.Do(ctx, http.MethodGet, path, "", nil, &response); err != nil {
			return mapTunnelError(err)
		}
		encoded, _ := json.Marshal(response)
		if string(encoded) != previous && renderer.Mode != output.Quiet {
			if renderer.Mode == output.JSON {
				_ = json.NewEncoder(renderer.Out).Encode(response)
			} else {
				_ = writeIndentedJSON(renderer.Out, response)
			}
			previous = string(encoded)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *App) roomInviteToFile(ctx context.Context, args []string, cfg config.Config, renderer output.Renderer) error {
	out, clean, err := extractValueOption(args, "--out")
	if err != nil || strings.TrimSpace(out) == "" {
		return usage("room invite --out requires a file path")
	}
	var stdout, stderr bytes.Buffer
	jsonRenderer := output.Renderer{Mode: output.JSON, Out: &stdout, Err: &stderr}
	if err := a.roomRun(ctx, clean, cfg.AgentSocket, a.version.Version, jsonRenderer); err != nil {
		return err
	}
	var invitation roommanager.InvitationResult
	if err := json.Unmarshal(stdout.Bytes(), &invitation); err != nil || invitation.InvitationToken == "" {
		return cliError(ExitOperationFailed, "Room invitation did not return a token.", "Try creating the invitation again.", err)
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return usage("invitation output path is invalid")
	}
	if err := writeSecretFile(absolute, invitation.InvitationToken+"\n"); err != nil {
		return cliError(ExitOperationFailed, "Could not write the invitation file.", "Choose a new owner-writable path.", err)
	}
	result := map[string]any{"invitation": invitation.Invitation, "invitation_file": absolute}
	return renderer.Result(result, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Invitation %s written securely to %s.\n", invitation.Invitation.InvitationID, absolute)
		return err
	})
}

func extractValueOption(args []string, name string) (string, []string, error) {
	var value string
	clean := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == name {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
				return "", nil, usage(name + " requires a value")
			}
			value = args[index+1]
			index++
			continue
		}
		if strings.HasPrefix(args[index], name+"=") {
			value = strings.TrimPrefix(args[index], name+"=")
			continue
		}
		clean = append(clean, args[index])
	}
	return value, clean, nil
}

func writeSecretFile(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, value); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func roomPathForCommand(roomID string) string {
	return "/v1/rooms/" + url.PathEscape(roomID)
}

func writeIndentedJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
