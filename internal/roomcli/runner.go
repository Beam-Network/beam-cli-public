package roomcli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btr"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btradapter"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/roommanager"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/state"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

const (
	// v14 is the first local API that carries the Beam API key paying for a
	// room. An older daemon cannot name a payer, so room commands refuse it here
	// instead of letting the coordinator answer "unauthorized".
	MinimumLocalAPIVersion = 14

	ExitOK            = 0
	ExitUsage         = 2
	ExitDaemon        = 3
	ExitCompatibility = 4
	ExitNotFound      = 5
	ExitConflict      = 6
	ExitDenied        = 7
	ExitFailed        = 8
)

type Runner struct {
	Client *tunnel.LocalClient
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	// Styled reports whether Out is an interactive terminal. It carries the same
	// meaning as output.Renderer.OutInteractive and must stay false whenever the
	// result stream is piped or redirected, so a script reads plain text.
	Styled bool
	// Notices receives guidance meant for a person, such as the live-only
	// retention reminder. It defaults to Err. A caller that buffers Err to build
	// its error message points this at the terminal so notices show while a
	// long-running command such as listen is still going.
	Notices io.Writer
}

// listing renders a batch of rows as an aligned table. Under --quiet it prints
// the first column alone, because that mode exists for a shell consuming bare
// identifiers.
func (runner Runner) listing(mode outputMode, table output.Table) error {
	if mode.Quiet {
		for _, row := range table.Rows {
			if len(row) > 0 {
				_, _ = fmt.Fprintln(runner.out(), row[0])
			}
		}
		return nil
	}
	return output.WriteTable(runner.out(), table, runner.Styled)
}

type outputMode struct {
	JSON  bool
	Quiet bool
}

// Run executes a room command and reports a failure on the error stream,
// followed by a hint when the failure is one the CLI can explain.
func (runner Runner) Run(ctx context.Context, args []string) int {
	failure := runner.Execute(ctx, args)
	if failure == nil {
		return ExitOK
	}
	if failure.Message != "" {
		_, _ = fmt.Fprintf(runner.err(), "beam room: %s\n", failure.Message)
		if failure.Hint != "" {
			_, _ = fmt.Fprintf(runner.err(), "  hint: %s\n", failure.Hint)
		}
	}
	return failure.Code
}

// Execute runs a room command and returns nil on success. Usage text is
// written to the error stream; every other failure is returned for the caller
// to render.
func (runner Runner) Execute(ctx context.Context, args []string) *Failure {
	if runner.Client == nil {
		return &Failure{Code: ExitDaemon, Message: "local daemon client is unavailable"}
	}
	args, mode := extractOutputMode(args)
	if len(args) < 3 || args[0] != "tunnel" || args[1] != "room" {
		runner.usage()
		return &Failure{Code: ExitUsage}
	}
	var err error
	switch args[2] {
	case "list":
		err = runner.list(ctx, mode, args[3:])
	case "inspect":
		err = runner.inspect(ctx, mode, args[3:])
	case "create":
		err = runner.create(ctx, mode, args[3:])
	case "join":
		err = runner.join(ctx, mode, args[3:])
	case "leave":
		err = runner.leave(ctx, mode, args[3:])
	case "close":
		err = runner.close(ctx, mode, args[3:])
	case "refresh":
		err = runner.refresh(ctx, mode, args[3:])
	case "invite":
		err = runner.invite(ctx, mode, args[3:])
	case "members":
		err = runner.members(ctx, mode, args[3:])
	case "role":
		err = runner.role(ctx, mode, args[3:])
	case "channel":
		err = runner.channel(ctx, mode, args[3:])
	case "grant":
		err = runner.grant(ctx, mode, args[3:])
	default:
		runner.usage()
		return &Failure{Code: ExitUsage}
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, errUsage) {
		return &Failure{Code: ExitUsage}
	}
	failure := &Failure{Code: exitCode(err), Message: err.Error()}
	if known, ok := explainError(roomCommandName(args), err); ok {
		if known.DiagnoseChannel {
			known = runner.diagnoseChannel(ctx, args, known)
		}
		if known.DiagnoseDelivery {
			known = runner.diagnoseDelivery(ctx, args, err, known)
		}
		failure.Kind, failure.Hint = known.Kind, withTarget(known.Hint, args)
		if known.Message != "" {
			failure.Message = known.Message
		}
	}
	return failure
}

func (runner Runner) list(ctx context.Context, mode outputMode, args []string) error {
	flags := runner.flags("list")
	// Closed rooms are retained by the daemon rather than deleted, so without a
	// default they accumulate in the listing indefinitely. Only terminal rooms
	// are hidden: a suspended room is still live and stays visible.
	stateFilter := flags.String("state", "", "show only rooms in this state, or \"all\" to include closed rooms")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	var response struct {
		Rooms []state.BTRRoomRecord `json:"rooms"`
	}
	if err := runner.Client.Do(ctx, http.MethodGet, "/v1/rooms", "", nil, &response); err != nil {
		return err
	}
	selected := make([]state.BTRRoomRecord, 0, len(response.Rooms))
	for _, record := range response.Rooms {
		if roomStateSelected(string(record.Room.State), *stateFilter) {
			selected = append(selected, record)
		}
	}
	hidden := len(response.Rooms) - len(selected)
	response.Rooms = selected
	if mode.JSON {
		return runner.json(response)
	}
	// The authorization epoch and notification cursor are protocol bookkeeping:
	// you consult them when a mutation returns a conflict, not when scanning for
	// a room. They stay in "room <id> show" and in --json, where a caller that
	// wants them can ask. Dropping them here also keeps the line readable, since
	// full btr_room_* and btr_member_* identifiers already run past 100 columns.
	rows := make([][]string, 0, len(response.Rooms))
	for _, record := range response.Rooms {
		rows = append(rows, []string{
			record.Room.RoomID,
			string(record.Room.State),
			organizationColumn(record.Room.OrganizationID),
			record.Membership.MemberID,
		})
	}
	if err := runner.listing(mode, output.Table{
		Headers: []string{"ROOM ID", "STATE", "ORGANIZATION", "MEMBER"},
		Rows:    rows,
		Style:   output.RowStyle(1),
	}); err != nil {
		return err
	}
	if len(response.Rooms) == 0 && !mode.Quiet {
		runner.emptyListNotice(hidden, *stateFilter)
	}
	return nil
}

// emptyListNotice explains a blank listing on the status stream. A listing with
// no rows is ambiguous on its own: an account holding no rooms and an account
// whose rooms are all closed both print nothing at all. The note also carries
// the only mention of --state most callers will ever see.
//
// It names the filter that would reveal the hidden rooms instead of validating
// --state against a fixed vocabulary, which would reject states that only the
// coordinator knows about. That keeps a mistyped filter explained rather than
// silent, without teaching the CLI a room state vocabulary it does not own.
//
// The notice goes to the status stream and leaves the exit code at ExitOK, so
// an empty listing stays an empty listing for scripts reading stdout.
func (runner Runner) emptyListNotice(hidden int, filter string) {
	filter = strings.ToLower(strings.TrimSpace(filter))
	switch {
	case hidden == 0:
		_, _ = fmt.Fprintln(runner.err(), "beam room: no rooms.")
		_, _ = fmt.Fprintln(runner.err(), `  Create one with "beam room create --api-key $BEAM_API_KEY", or join one with "beam room join ROOM_ID --invitation-file FILE".`)
	case filter == "":
		// The default filter hides exactly the closed rooms, so the hidden count
		// can name them rather than describing the filter in the abstract.
		_, _ = fmt.Fprintf(runner.err(), "beam room: no open rooms (%s hidden).\n", countedRooms(hidden, "closed room"))
		_, _ = fmt.Fprintln(runner.err(), `  Use "beam room list --state all" to include them.`)
	default:
		_, _ = fmt.Fprintf(runner.err(), "beam room: no rooms in state %q (%s hidden).\n", filter, countedRooms(hidden, "room"))
		_, _ = fmt.Fprintln(runner.err(), `  Use "beam room list --state all" to list every room.`)
	}
}

func countedRooms(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

func (runner Runner) inspect(ctx context.Context, mode outputMode, args []string) error {
	if len(args) != 1 {
		return runner.usageError("inspect requires ROOM_ID")
	}
	var record state.BTRRoomRecord
	if err := runner.Client.Do(ctx, http.MethodGet, roomPath(args[0]), "", nil, &record); err != nil {
		return err
	}
	return runner.renderRoom(mode, record)
}

func (runner Runner) create(ctx context.Context, mode outputMode, args []string) error {
	flags := runner.flags("create")
	leaseTTL := flags.Int("lease-ttl", 60, "membership lease TTL in seconds")
	key := flags.String("idempotency-key", "", "stable retry key")
	apiKey := flags.String("api-key", "", "Beam API key charged for the room (default $BEAM_API_KEY)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	// Starting a room is billable. The agent identity says who is asking; the
	// Beam API key says who pays, and an organization may hold several keys with
	// different caps, so it is named rather than guessed.
	payer := strings.TrimSpace(*apiKey)
	if payer == "" {
		payer = strings.TrimSpace(os.Getenv("BEAM_API_KEY"))
	}
	if payer == "" {
		return runner.usageError("create requires --api-key or BEAM_API_KEY: starting a room is billable")
	}
	var record state.BTRRoomRecord
	body := map[string]any{"lease_ttl_seconds": *leaseTTL, "api_key": payer}
	if err := runner.Client.Do(ctx, http.MethodPost, "/v1/rooms", *key, body, &record); err != nil {
		return err
	}
	return runner.renderRoom(mode, record)
}

func (runner Runner) join(ctx context.Context, mode outputMode, args []string) error {
	roomID, rest, err := requiredID(args, "join requires ROOM_ID")
	if err != nil {
		return runner.usageError(err.Error())
	}
	flags := runner.flags("join")
	invitation := flags.String("invitation-token", "", "single-use invitation bearer")
	invitationFile := flags.String("invitation-file", "", "mode-0600 file containing a single-use invitation bearer")
	leaseTTL := flags.Int("lease-ttl", 60, "membership lease TTL in seconds")
	key := flags.String("idempotency-key", "", "stable retry key")
	if err := flags.Parse(rest); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	if strings.TrimSpace(*invitation) != "" && strings.TrimSpace(*invitationFile) != "" {
		return runner.usageError("join accepts only one of --invitation-token or --invitation-file")
	}
	if strings.TrimSpace(*invitationFile) != "" {
		info, statErr := os.Lstat(*invitationFile)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > btr.MaxTokenBytes {
			return runner.usageError("invitation file must be a non-empty regular file within the token size limit")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return runner.usageError("invitation file must not be accessible by group or other users")
		}
		payload, readErr := os.ReadFile(*invitationFile)
		if readErr != nil {
			return runner.usageError(fmt.Sprintf("read invitation file: %s", readErr))
		}
		*invitation = strings.TrimSpace(string(payload))
	}
	if strings.TrimSpace(*invitation) == "" {
		return runner.usageError("join requires --invitation-token or --invitation-file")
	}
	var record state.BTRRoomRecord
	body := map[string]any{"invitation_token": *invitation, "lease_ttl_seconds": *leaseTTL}
	if err := runner.Client.Do(ctx, http.MethodPost, roomPath(roomID)+"/join", *key, body, &record); err != nil {
		return err
	}
	return runner.renderRoom(mode, record)
}

func (runner Runner) leave(ctx context.Context, mode outputMode, args []string) error {
	roomID, rest, err := requiredID(args, "leave requires ROOM_ID")
	if err != nil {
		return runner.usageError(err.Error())
	}
	flags := runner.flags("leave")
	key := flags.String("idempotency-key", "", "stable retry key")
	if err := flags.Parse(rest); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	var record state.BTRRoomRecord
	if err := runner.Client.Do(ctx, http.MethodPost, roomPath(roomID)+"/leave", *key, nil, &record); err != nil {
		return err
	}
	return runner.renderRoom(mode, record)
}

func (runner Runner) close(ctx context.Context, mode outputMode, args []string) error {
	roomID, rest, err := requiredID(args, "close requires ROOM_ID")
	if err != nil {
		return runner.usageError(err.Error())
	}
	flags := runner.flags("close")
	key := flags.String("idempotency-key", "", "stable retry key")
	if err := flags.Parse(rest); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	var record state.BTRRoomRecord
	if err := runner.Client.Do(ctx, http.MethodPost, roomPath(roomID)+"/close", *key, nil, &record); err != nil {
		return err
	}
	return runner.renderRoom(mode, record)
}

func (runner Runner) refresh(ctx context.Context, mode outputMode, args []string) error {
	if len(args) != 1 {
		return runner.usageError("refresh requires ROOM_ID")
	}
	var record state.BTRRoomRecord
	if err := runner.Client.Do(ctx, http.MethodPost, roomPath(args[0])+"/refresh", "", nil, &record); err != nil {
		return err
	}
	return runner.renderRoom(mode, record)
}

func (runner Runner) invite(ctx context.Context, mode outputMode, args []string) error {
	roomID, rest, err := requiredID(args, "invite requires ROOM_ID")
	if err != nil {
		return runner.usageError(err.Error())
	}
	flags := runner.flags("invite")
	principal := flags.String("principal", "", "bound principal ID")
	agent := flags.String("agent", "", "bound agent ID")
	maxUses := flags.Uint("max-uses", 1, "bounded invitation uses")
	ttl := flags.Int("ttl", 900, "invitation TTL in seconds")
	key := flags.String("idempotency-key", "", "stable retry key")
	var roleIDs, channelAccessValues repeatedValues
	flags.Var(&roleIDs, "role", "role ID granted on join (repeatable)")
	flags.Var(&channelAccessValues, "channel-access", "CHANNEL_ID=ACTION,ACTION granted on join (repeatable)")
	if err := flags.Parse(rest); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	channelAccess, err := parseInvitationChannelAccess(channelAccessValues)
	if err != nil {
		return runner.usageError(err.Error())
	}
	var result roommanager.InvitationResult
	body := map[string]any{"bound_principal_id": *principal, "bound_agent_id": *agent, "max_uses": *maxUses, "ttl_seconds": *ttl, "role_ids": []string(roleIDs), "channel_access": channelAccess}
	if err := runner.Client.Do(ctx, http.MethodPost, roomPath(roomID)+"/invitations", *key, body, &result); err != nil {
		return err
	}
	if mode.JSON {
		return runner.json(result)
	}
	if mode.Quiet {
		_, _ = fmt.Fprintln(runner.out(), result.InvitationToken)
	} else {
		_, _ = fmt.Fprintf(runner.out(), "Invitation %s (expires %s)\n%s\n", result.Invitation.InvitationID, result.Invitation.ExpiresAt.Format(time.RFC3339), result.InvitationToken)
	}
	return nil
}

func parseInvitationChannelAccess(values []string) ([]btr.InvitationChannelAccess, error) {
	access := make([]btr.InvitationChannelAccess, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		channelID, actionsValue, found := strings.Cut(value, "=")
		channelID = strings.TrimSpace(channelID)
		if !found || channelID == "" || strings.TrimSpace(actionsValue) == "" {
			return nil, fmt.Errorf("--channel-access requires CHANNEL_ID=ACTION,ACTION")
		}
		if _, exists := seen[channelID]; exists {
			return nil, fmt.Errorf("--channel-access repeats channel %s", channelID)
		}
		seen[channelID] = struct{}{}
		actions := make([]btr.Action, 0)
		for _, action := range strings.Split(actionsValue, ",") {
			action = strings.TrimSpace(action)
			if action == "" {
				return nil, fmt.Errorf("--channel-access contains an empty action")
			}
			actions = append(actions, btr.Action(action))
		}
		access = append(access, btr.InvitationChannelAccess{ChannelID: channelID, Actions: actions})
	}
	return access, nil
}

func (runner Runner) members(ctx context.Context, mode outputMode, args []string) error {
	if len(args) != 1 {
		return runner.usageError("members requires ROOM_ID")
	}
	var response struct {
		Memberships []btr.Membership `json:"memberships"`
	}
	if err := runner.Client.Do(ctx, http.MethodGet, roomPath(args[0])+"/memberships", "", nil, &response); err != nil {
		return err
	}
	if mode.JSON {
		return runner.json(response)
	}
	rows := make([][]string, 0, len(response.Memberships))
	for _, member := range response.Memberships {
		identity := member.AgentID
		if member.Kind == "object_storage" {
			identity = member.ResourceID
		}
		name := member.DisplayName
		if name == "" {
			name = identity
		}
		kind := member.Kind
		if kind == "" {
			kind = "agent"
		}
		rows = append(rows, []string{member.MemberID, name, kind, string(member.State), string(member.Presence), identity})
	}
	return runner.listing(mode, output.Table{
		Headers: []string{"MEMBER ID", "NAME", "TYPE", "STATE", "PRESENCE", "IDENTITY"},
		Rows:    rows,
		Style:   output.RowStyle(1),
	})
}

func (runner Runner) role(ctx context.Context, mode outputMode, args []string) error {
	if len(args) < 2 {
		return runner.usageError("role requires list, create, assign, or revoke plus ROOM_ID")
	}
	action, roomID, rest := args[0], args[1], args[2:]
	base := roomPath(roomID) + "/roles"
	switch action {
	case "list":
		if len(rest) != 0 {
			return errUsage
		}
		var response struct {
			Roles       []btr.Role       `json:"roles"`
			MemberRoles []btr.MemberRole `json:"member_roles"`
			Epoch       uint64           `json:"authorization_epoch"`
		}
		if err := runner.Client.Do(ctx, http.MethodGet, base, "", nil, &response); err != nil {
			return err
		}
		if mode.JSON {
			return runner.json(response)
		}
		rows := make([][]string, 0, len(response.Roles))
		for _, role := range response.Roles {
			rows = append(rows, []string{role.RoleID, string(role.Template), role.Name})
		}
		return runner.listing(mode, output.Table{
			Headers: []string{"ROLE ID", "TEMPLATE", "NAME"},
			Rows:    rows,
			Style:   output.RowStyle(-1),
		})
	case "create":
		flags := runner.flags("role create")
		name := flags.String("name", "", "role name")
		epoch := flags.Uint64("epoch", 0, "expected authorization epoch; defaults to local floor")
		key := flags.String("idempotency-key", "", "stable retry key")
		if err := flags.Parse(rest); err != nil || strings.TrimSpace(*name) == "" || flags.NArg() != 0 {
			return runner.usageError("role create requires --name")
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodPost, base, *key, map[string]any{"name": *name, "expected_authorization_epoch": *epoch}, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, mapString(response, "role", "role_id"))
	case "assign", "revoke":
		if len(rest) < 2 {
			return runner.usageError("role assign/revoke requires ROLE_ID MEMBER_ID")
		}
		roleID, memberID, tail := rest[0], rest[1], rest[2:]
		flags := runner.flags("role " + action)
		epoch := flags.Uint64("epoch", 0, "expected authorization epoch; defaults to local floor")
		key := flags.String("idempotency-key", "", "stable retry key")
		if err := flags.Parse(tail); err != nil || flags.NArg() != 0 {
			return errUsage
		}
		method := http.MethodPut
		if action == "revoke" {
			method = http.MethodDelete
		}
		path := base + "/" + url.PathEscape(roleID) + "/members/" + url.PathEscape(memberID)
		var response map[string]any
		if err := runner.Client.Do(ctx, method, path, *key, map[string]any{"expected_authorization_epoch": *epoch}, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, memberID)
	default:
		return runner.usageError("unknown role command " + action)
	}
}

func (runner Runner) channel(ctx context.Context, mode outputMode, args []string) error {
	if len(args) < 2 {
		return runner.usageError("channel requires list, inspect, create, update, close, publish, listen, or persistence plus ROOM_ID")
	}
	action, roomID, rest := args[0], args[1], args[2:]
	base := roomPath(roomID) + "/channels"
	switch action {
	case "list":
		var response struct {
			Channels []btr.Channel `json:"channels"`
			Epoch    uint64        `json:"authorization_epoch"`
		}
		if err := runner.Client.Do(ctx, http.MethodGet, base, "", nil, &response); err != nil {
			return err
		}
		if mode.JSON {
			return runner.json(response)
		}
		rows := make([][]string, 0, len(response.Channels))
		for _, channel := range response.Channels {
			rows = append(rows, []string{channel.ChannelID, string(channel.Kind), string(channel.State), channel.Name, strconv.FormatUint(channel.ChannelRevision, 10)})
		}
		return runner.listing(mode, output.Table{
			Headers: []string{"CHANNEL ID", "KIND", "STATE", "NAME", "REVISION"},
			Rows:    rows,
			Style:   output.RowStyle(2),
		})
	case "inspect":
		if len(rest) != 1 {
			return runner.usageError("channel inspect requires CHANNEL_ID")
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodGet, base+"/"+url.PathEscape(rest[0]), "", nil, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, rest[0])
	case "create":
		input, key, err := runner.channelInput(rest, false)
		if err != nil {
			return err
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodPost, base, key, input, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, mapString(response, "channel", "channel_id"))
	case "update":
		if len(rest) < 1 {
			return runner.usageError("channel update requires CHANNEL_ID")
		}
		channelID := rest[0]
		input, key, err := runner.channelInput(rest[1:], true)
		if err != nil {
			return err
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodPost, base+"/"+url.PathEscape(channelID)+"/policy", key, input, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, channelID)
	case "close":
		if len(rest) < 1 {
			return runner.usageError("channel close requires CHANNEL_ID")
		}
		channelID := rest[0]
		flags := runner.flags("channel close")
		epoch := flags.Uint64("epoch", 0, "expected authorization epoch; defaults to local floor")
		revision := flags.Uint64("revision", 0, "expected channel revision; defaults to local floor")
		key := flags.String("idempotency-key", "", "stable retry key")
		if err := flags.Parse(rest[1:]); err != nil || flags.NArg() != 0 {
			return errUsage
		}
		var response map[string]any
		body := map[string]any{"expected_authorization_epoch": *epoch, "expected_channel_revision": *revision}
		if err := runner.Client.Do(ctx, http.MethodPost, base+"/"+url.PathEscape(channelID)+"/close", *key, body, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, channelID)
	case "stream", "datagram":
		return runner.channelAdapter(ctx, mode, action, roomID, rest)
	case "object":
		return runner.channelObject(ctx, mode, args[1:])
	case "media":
		return runner.channelMedia(ctx, mode, args[1:])
	case "publish":
		if len(rest) < 1 {
			return runner.usageError("channel publish requires CHANNEL_ID")
		}
		channelID := rest[0]
		flags := runner.flags("channel publish")
		literal := flags.String("literal", "", "literal message payload")
		stdin := flags.Bool("stdin", false, "read message payload from stdin")
		contentType := flags.String("content-type", "text/plain", "application content type")
		relayID := flags.String("relay", "", "coordinator relay placement constraint")
		standbyRelayID := flags.String("standby-relay", "", "coordinator standby relay placement constraint")
		replace := flags.Bool("replace-primary", false, "replace the primary relay and advance plan_version")
		key := flags.String("idempotency-key", "", "stable publication retry key")
		retention := flags.String("retention", string(btr.PersistenceNone), "none (live only: only members listening now receive it) or sender_local retention")
		backend := flags.String("persistence-backend", "", "opaque agent-local persistence backend reference")
		if err := flags.Parse(rest[1:]); err != nil || flags.NArg() != 0 || (*stdin == (strings.TrimSpace(*literal) != "")) {
			return runner.usageError("channel publish requires exactly one of --literal or --stdin")
		}
		selection, err := persistenceSelection(*retention, *backend, btr.PersistenceSenderLocal)
		if err != nil {
			return runner.usageError(err.Error())
		}
		body := map[string]any{"content_type": *contentType, "relay_id": *relayID, "standby_relay_id": *standbyRelayID, "replace_primary": *replace, "persistence": selection}
		if *stdin {
			payload, err := io.ReadAll(io.LimitReader(runner.in(), btr.MaxMessageCiphertextBytes+1))
			if err != nil {
				return err
			}
			if len(payload) > btr.MaxMessageCiphertextBytes {
				return runner.usageError("stdin payload exceeds 1 MiB")
			}
			body["payload_base64"] = base64.StdEncoding.EncodeToString(payload)
		} else {
			body["literal"] = *literal
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodPost, base+"/"+url.PathEscape(channelID)+"/publish", *key, body, &response); err != nil {
			return err
		}
		if err := runner.renderGeneric(mode, response, fmt.Sprint(response["publication_id"])); err != nil {
			return err
		}
		if selection.Mode == btr.PersistenceNone {
			if text := liveOnlyPublishNotice(response); text != "" {
				runner.notice(mode, text)
			}
		}
		return nil
	case "listen":
		if len(rest) < 1 {
			return runner.usageError("channel listen requires CHANNEL_ID")
		}
		channelID := rest[0]
		flags := runner.flags("channel listen")
		relayID := flags.String("relay", "", "coordinator relay placement constraint")
		standbyRelayID := flags.String("standby-relay", "", "coordinator standby relay placement constraint")
		replace := flags.Bool("replace-primary", false, "replace the primary relay and advance plan_version")
		retention := flags.String("retention", string(btr.PersistenceNone), "none (live only: only messages published while listening) or receiver_local retention")
		backend := flags.String("persistence-backend", "", "opaque agent-local persistence backend reference")
		if err := flags.Parse(rest[1:]); err != nil || flags.NArg() != 0 {
			return errUsage
		}
		query := url.Values{}
		if *relayID != "" {
			query.Set("relay_id", *relayID)
		}
		if *standbyRelayID != "" {
			query.Set("standby_relay_id", *standbyRelayID)
		}
		if *replace {
			query.Set("replace_primary", "true")
		}
		selection, err := persistenceSelection(*retention, *backend, btr.PersistenceReceiverLocal)
		if err != nil {
			return runner.usageError(err.Error())
		}
		query.Set("retention", string(selection.Mode))
		if selection.BackendRef != "" {
			query.Set("persistence_backend", selection.BackendRef)
		}
		path := base + "/" + url.PathEscape(channelID) + "/listen"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		// The notice goes out before the request: an agent may hold the
		// response headers until the first message arrives, and the reminder
		// matters most while nothing has arrived yet.
		if selection.Mode == btr.PersistenceNone {
			runner.notice(mode, liveOnlyListenNotice)
		}
		response, err := runner.Client.OpenStream(ctx, path)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		decoder := json.NewDecoder(response.Body)
		for received := 0; ; received++ {
			var delivery map[string]any
			if err := decoder.Decode(&delivery); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// The daemon ends the stream without an error when this member
				// may not subscribe or the channel is not ready, which used to
				// look like a successful, empty listen.
				if errors.Is(err, io.EOF) && received == 0 {
					return &tunnel.Error{Kind: tunnel.ErrResponse, Code: listenClosedCode, Detail: "the listen stream ended before any message arrived"}
				}
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if mode.JSON {
				if err := runner.json(delivery); err != nil {
					return err
				}
			} else if mode.Quiet {
				_, _ = fmt.Fprintln(runner.out(), mapStringFlat(delivery, "publication_id"))
			} else if text, ok := delivery["text"].(string); ok {
				_, _ = fmt.Fprintln(runner.out(), text)
			} else {
				payload, _ := delivery["payload"].(string)
				_, _ = fmt.Fprintln(runner.out(), payload)
			}
		}
	case "persistence":
		return runner.channelPersistence(ctx, mode, base, rest)
	default:
		return runner.usageError("unknown channel command " + action)
	}
}

func (runner Runner) channelPersistence(ctx context.Context, mode outputMode, channelsBase string, args []string) error {
	if len(args) < 2 {
		return runner.usageError("channel persistence requires CHANNEL_ID and status, retry, acknowledge, or purge")
	}
	channelID, action := args[0], args[1]
	base := channelsBase + "/" + url.PathEscape(channelID) + "/persistence"
	flags := runner.flags("channel persistence " + action)
	kind := flags.String("kind", "", "outbox or inbox")
	recordID := flags.String("record", "", "exact local record id")
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	var response map[string]any
	switch action {
	case "status":
		query := url.Values{}
		if *kind != "" {
			if *kind != "outbox" && *kind != "inbox" {
				return runner.usageError("--kind must be outbox or inbox")
			}
			query.Set("kind", *kind)
		}
		path := base
		if query.Encode() != "" {
			path += "?" + query.Encode()
		}
		if err := runner.Client.Do(ctx, http.MethodGet, path, "", nil, &response); err != nil {
			return err
		}
	case "retry", "acknowledge":
		if strings.TrimSpace(*recordID) == "" {
			return runner.usageError("--record is required")
		}
		endpoint := "retry"
		if action == "acknowledge" {
			endpoint = "acknowledge"
		}
		if err := runner.Client.Do(ctx, http.MethodPost, base+"/"+endpoint, "", map[string]string{"record_id": *recordID}, &response); err != nil {
			return err
		}
	case "purge":
		if *kind != "outbox" && *kind != "inbox" {
			return runner.usageError("purge requires --kind outbox or --kind inbox")
		}
		if err := runner.Client.Do(ctx, http.MethodDelete, base+"/"+url.PathEscape(*kind), "", nil, &response); err != nil {
			return err
		}
	default:
		return runner.usageError("unknown channel persistence command " + action)
	}
	return runner.renderGeneric(mode, response, fmt.Sprint(response["pending_records"]))
}

func persistenceSelection(rawMode, backend string, allowedLocal btr.PersistenceMode) (btr.LocalPersistenceSelection, error) {
	selection := btr.LocalPersistenceSelection{Mode: btr.PersistenceMode(strings.TrimSpace(rawMode)), BackendRef: strings.TrimSpace(backend)}
	if selection.Mode != btr.PersistenceNone && selection.Mode != allowedLocal {
		return btr.LocalPersistenceSelection{}, fmt.Errorf("retention must be none or %s", allowedLocal)
	}
	if selection.Mode == btr.PersistenceNone {
		if selection.BackendRef != "" {
			return btr.LocalPersistenceSelection{}, errors.New("none retention cannot select a persistence backend")
		}
		return selection, nil
	}
	if selection.BackendRef == "" {
		selection.BackendRef = "local-encrypted"
	}
	return selection, nil
}

func (runner Runner) grant(ctx context.Context, mode outputMode, args []string) error {
	if len(args) < 3 {
		return runner.usageError("grant requires list, put, or revoke plus ROOM_ID CHANNEL_ID")
	}
	action, roomID, channelID, rest := args[0], args[1], args[2], args[3:]
	base := roomPath(roomID) + "/channels/" + url.PathEscape(channelID) + "/grants"
	switch action {
	case "list":
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodGet, base, "", nil, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, channelID)
	case "put":
		flags := runner.flags("grant put")
		subjectType := flags.String("subject-type", "", "role or member")
		subjectID := flags.String("subject", "", "role or member ID")
		actions := flags.String("actions", "", "comma-separated exact actions")
		epoch := flags.Uint64("epoch", 0, "expected authorization epoch; defaults to local floor")
		revision := flags.Uint64("revision", 0, "expected channel revision; defaults to local floor")
		key := flags.String("idempotency-key", "", "stable retry key")
		if err := flags.Parse(rest); err != nil || *subjectType == "" || *subjectID == "" || *actions == "" || flags.NArg() != 0 {
			return runner.usageError("grant put requires --subject-type, --subject, and --actions")
		}
		body := roommanager.PutGrantInput{
			SubjectType: btr.GrantSubjectType(*subjectType), SubjectID: *subjectID, Actions: parseActions(*actions),
			ExpectedAuthorizationEpoch: *epoch, ExpectedChannelRevision: *revision,
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodPut, base, *key, body, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, mapString(response, "grant", "grant_id"))
	case "revoke":
		if len(rest) < 1 {
			return runner.usageError("grant revoke requires GRANT_ID")
		}
		grantID := rest[0]
		flags := runner.flags("grant revoke")
		epoch := flags.Uint64("epoch", 0, "expected authorization epoch; defaults to local floor")
		revision := flags.Uint64("revision", 0, "expected channel revision; defaults to local floor")
		key := flags.String("idempotency-key", "", "stable retry key")
		if err := flags.Parse(rest[1:]); err != nil || flags.NArg() != 0 {
			return errUsage
		}
		body := map[string]any{"expected_authorization_epoch": *epoch, "expected_channel_revision": *revision}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodDelete, base+"/"+url.PathEscape(grantID), *key, body, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, grantID)
	default:
		return runner.usageError("unknown grant command " + action)
	}
}

func (runner Runner) channelInput(args []string, update bool) (any, string, error) {
	flags := runner.flags("channel policy")
	name := flags.String("name", "", "channel name")
	kind := flags.String("kind", "message", "channel transport kind")
	contentType := flags.String("content-type", "application/json", "opaque application content type")
	schemaRef := flags.String("schema-ref", "", "opaque schema reference")
	visibility := flags.String("visibility", "restricted", "room or restricted")
	reliability := flags.String("reliability", "", "reliable or best_effort; defaults by channel kind")
	acknowledgement := flags.String("acknowledgement", "", "none, accepted, or delivered; defaults by channel kind")
	backpressure := flags.String("backpressure", "", "reject, disconnect, or drop_oldest; defaults by channel kind")
	ordering := flags.String("ordering", "", "none, per_publisher, or per_flow; defaults by channel kind")
	qosClass := flags.String("qos-class", "", "bulk, standard, or interactive; defaults by channel kind")
	maxLatency := flags.Duration("max-latency", 0, "accepted QoS latency target; defaults by channel kind")
	preferredRate := flags.Uint64("preferred-rate", 0, "accepted QoS preferred bytes per second; defaults by channel kind")
	persistenceDefault := flags.String("persistence-default", string(btr.PersistenceNone), "default retention mode")
	persistenceModes := flags.String("persistence-modes", string(btr.PersistenceNone), "comma-separated allowed retention modes")
	persistenceMaxBytes := flags.Uint64("persistence-max-bytes", 0, "per-channel local retention byte quota")
	persistenceMaxAge := flags.Duration("persistence-max-age", 0, "maximum local retention age")
	maxPayload := flags.Uint64("max-payload", 0, "declared maximum payload bytes; defaults by channel kind")
	maxRate := flags.Uint64("max-rate", 1<<20, "maximum accepted bytes per second")
	maxInflight := flags.Uint("max-inflight", 16, "maximum concurrent publications or flows")
	maxQueueMessages := flags.Uint("max-queue-messages", 64, "maximum queued records per subscriber")
	maxQueueBytes := flags.Uint64("max-queue-bytes", 1<<20, "maximum queued bytes per subscriber")
	maxSubscribers := flags.Uint("max-subscribers", 64, "maximum concurrent subscribers")
	maxFanout := flags.Uint("max-fanout", 4, "maximum relay branch fanout")
	maxTreeDepth := flags.Uint("max-tree-depth", 4, "maximum relay tree depth")
	deduplicationWindow := flags.Duration("deduplication-window", time.Minute, "message publication deduplication window")
	publicationTTL := flags.Duration("publication-ttl", time.Minute, "maximum publication or flow lifetime")
	epoch := flags.Uint64("epoch", 0, "expected authorization epoch; defaults to local floor")
	revision := flags.Uint64("revision", 0, "expected channel revision; defaults to local floor")
	key := flags.String("idempotency-key", "", "stable retry key")
	if err := flags.Parse(args); err != nil || strings.TrimSpace(*name) == "" || flags.NArg() != 0 {
		return nil, "", runner.usageError("channel create/update requires --name")
	}
	allowedModes, err := parsePersistenceModes(*persistenceModes)
	if err != nil {
		return nil, "", runner.usageError(err.Error())
	}
	defaultMode := btr.PersistenceMode(strings.TrimSpace(*persistenceDefault))
	if !slices.Contains(allowedModes, defaultMode) {
		return nil, "", runner.usageError("persistence default must be in persistence modes")
	}
	hasLocal := slices.Contains(allowedModes, btr.PersistenceSenderLocal) || slices.Contains(allowedModes, btr.PersistenceReceiverLocal)
	if hasLocal && (*persistenceMaxBytes == 0 || *persistenceMaxAge <= 0) {
		return nil, "", runner.usageError("local persistence modes require positive --persistence-max-bytes and --persistence-max-age")
	}
	if !hasLocal && (*persistenceMaxBytes != 0 || *persistenceMaxAge != 0) {
		return nil, "", runner.usageError("none-only persistence cannot set local limits")
	}
	kindValue := btr.ChannelKind(strings.TrimSpace(*kind))
	defaultOrdering, defaultQoS, defaultLatency, defaultPreferredRate := btr.OrderingPerPublisher, btr.QoSStandard, time.Second, uint64(1<<20)
	defaultReliability, defaultAcknowledgement, defaultBackpressure := btr.DeliveryReliable, btr.AcknowledgementAccepted, btr.BackpressureDisconnect
	defaultMaxPayload := uint64(btr.MaxMessageCiphertextBytes)
	switch kindValue {
	case btr.ChannelKindMessage:
	case btr.ChannelKindStream:
		defaultOrdering, defaultQoS, defaultMaxPayload = btr.OrderingPerFlow, btr.QoSInteractive, btr.MaxStreamRecordBytes
		defaultAcknowledgement = btr.AcknowledgementNone
	case btr.ChannelKindDatagram:
		defaultOrdering, defaultQoS, defaultMaxPayload = btr.OrderingNone, btr.QoSInteractive, btradapter.MaxDatagramPayloadBytes
		defaultReliability, defaultAcknowledgement, defaultBackpressure = btr.DeliveryBestEffort, btr.AcknowledgementNone, btr.BackpressureDropOldest
	case btr.ChannelKindRequestReply:
		defaultAcknowledgement = btr.AcknowledgementDelivered
	case btr.ChannelKindObject:
		defaultOrdering, defaultQoS, defaultLatency, defaultPreferredRate = btr.OrderingPerFlow, btr.QoSBulk, 5*time.Second, 8<<20
		defaultAcknowledgement = btr.AcknowledgementDelivered
	case btr.ChannelKindMedia:
		defaultOrdering, defaultQoS, defaultMaxPayload = btr.OrderingPerFlow, btr.QoSInteractive, btr.MaxStreamRecordBytes
		defaultReliability, defaultAcknowledgement, defaultBackpressure = btr.DeliveryBestEffort, btr.AcknowledgementNone, btr.BackpressureDropOldest
	default:
		return nil, "", runner.usageError("unsupported channel kind " + string(kindValue))
	}
	if *ordering == "" {
		*ordering = string(defaultOrdering)
	}
	if *qosClass == "" {
		*qosClass = string(defaultQoS)
	}
	if *maxLatency == 0 {
		*maxLatency = defaultLatency
	}
	if *preferredRate == 0 {
		*preferredRate = defaultPreferredRate
	}
	if *reliability == "" {
		*reliability = string(defaultReliability)
	}
	if *acknowledgement == "" {
		*acknowledgement = string(defaultAcknowledgement)
	}
	if *backpressure == "" {
		*backpressure = string(defaultBackpressure)
	}
	if *maxPayload == 0 {
		*maxPayload = defaultMaxPayload
	}
	if *maxPayload > defaultMaxPayload || *maxRate == 0 || *maxInflight == 0 || uint64(*maxInflight) > uint64(^uint32(0)) ||
		*maxQueueMessages == 0 || uint64(*maxQueueMessages) > uint64(^uint32(0)) || *maxQueueBytes == 0 ||
		*maxSubscribers == 0 || uint64(*maxSubscribers) > uint64(^uint32(0)) || *maxFanout == 0 || uint64(*maxFanout) > uint64(^uint32(0)) ||
		*maxTreeDepth == 0 || uint64(*maxTreeDepth) > uint64(^uint32(0)) || *deduplicationWindow <= 0 || *publicationTTL <= 0 || *maxLatency <= 0 {
		return nil, "", runner.usageError("channel limits must be positive and within the selected adapter bounds")
	}
	policy := roommanager.ChannelPolicyInput{
		Name: *name, Kind: kindValue, ContentType: *contentType, SchemaRef: *schemaRef,
		Visibility: btr.ChannelVisibility(*visibility),
		Delivery: btr.DeliveryPolicy{Reliability: btr.DeliveryReliability(*reliability),
			Acknowledgement: btr.AcknowledgementPolicy(*acknowledgement), Backpressure: btr.BackpressurePolicy(*backpressure)},
		Persistence: roommanager.PersistenceInput{Mode: defaultMode, AllowedModes: allowedModes, MaxBytes: *persistenceMaxBytes, MaxAgeSeconds: int64((*persistenceMaxAge) / time.Second)}, Ordering: btr.OrderingPolicy(*ordering),
		QoS: roommanager.QoSInput{Class: btr.QoSClass(*qosClass), MaxLatencyMillis: int64((*maxLatency) / time.Millisecond), PreferredRateBPS: *preferredRate},
		Limits: roommanager.LimitsInput{
			MaxPayloadBytes: *maxPayload, MaxRateBPS: *maxRate, MaxInflight: uint32(*maxInflight),
			MaxQueueMessages: uint32(*maxQueueMessages), MaxQueueBytes: *maxQueueBytes, MaxSubscribers: uint32(*maxSubscribers),
			MaxFanoutDegree: uint32(*maxFanout), MaxTreeDepth: uint32(*maxTreeDepth), DeduplicationWindowSeconds: int64((*deduplicationWindow) / time.Second),
			PublicationTTLSeconds: int64((*publicationTTL) / time.Second),
		},
	}
	if update {
		return roommanager.UpdateChannelInput{ChannelPolicyInput: policy, ExpectedAuthorizationEpoch: *epoch, ExpectedChannelRevision: *revision}, *key, nil
	}
	return roommanager.CreateChannelInput{ChannelPolicyInput: policy, ExpectedAuthorizationEpoch: *epoch}, *key, nil
}

func parsePersistenceModes(raw string) ([]btr.PersistenceMode, error) {
	seen := make(map[btr.PersistenceMode]struct{})
	result := make([]btr.PersistenceMode, 0, 3)
	for _, item := range strings.Split(raw, ",") {
		mode := btr.PersistenceMode(strings.TrimSpace(item))
		if mode != btr.PersistenceNone && mode != btr.PersistenceSenderLocal && mode != btr.PersistenceReceiverLocal {
			return nil, fmt.Errorf("unsupported persistence mode %q", mode)
		}
		if _, duplicate := seen[mode]; duplicate {
			return nil, fmt.Errorf("duplicate persistence mode %q", mode)
		}
		seen[mode] = struct{}{}
		result = append(result, mode)
	}
	if len(result) == 0 {
		return nil, errors.New("at least one persistence mode is required")
	}
	return result, nil
}

func (runner Runner) renderRoom(mode outputMode, record state.BTRRoomRecord) error {
	if mode.JSON {
		return runner.json(record)
	}
	if mode.Quiet {
		_, _ = fmt.Fprintln(runner.out(), record.Room.RoomID)
		return nil
	}
	_, _ = fmt.Fprintf(runner.out(), "Room: %s\nState: %s\nOrganization: %s\nMember: %s\nAuthorization epoch: %d\nNotification cursor: %d\n", record.Room.RoomID, record.Room.State, organizationColumn(record.Room.OrganizationID), record.Membership.MemberID, record.AuthorizationEpoch, record.Resume.NotificationCursor)
	return nil
}

// roomStateSelected reports whether a room belongs in the listing. An empty
// filter keeps everything except closed rooms, "all" keeps everything, and any
// other value matches that state exactly.
func roomStateSelected(roomState, filter string) bool {
	roomState = strings.ToLower(strings.TrimSpace(roomState))
	switch strings.ToLower(strings.TrimSpace(filter)) {
	case "":
		return roomState != "closed"
	case "all":
		return true
	case roomState:
		return true
	default:
		return false
	}
}

// organizationColumn keeps room output aligned when the coordinator returns no
// organization, which happens for records written before rooms carried one.
func organizationColumn(organizationID string) string {
	if strings.TrimSpace(organizationID) == "" {
		return "-"
	}
	return organizationID
}

func (runner Runner) renderGeneric(mode outputMode, value any, quiet string) error {
	if mode.JSON {
		return runner.json(value)
	}
	if mode.Quiet {
		_, _ = fmt.Fprintln(runner.out(), quiet)
		return nil
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(runner.out(), string(payload))
	return nil
}

// Retention none is live only. Nothing fails when a message is published
// before its recipients listen; it is simply never delivered to them. These
// notices make that visible to a person without changing what a script reads.
const (
	liveOnlyPublishHint  = "  hint: start `listen` on the receiving side before publishing, or publish with --retention sender_local if the channel allows it."
	liveOnlyListenNotice = "beam room: listening live only (--retention none): messages published before now are not replayed.\n" +
		"  hint: use --retention receiver_local to keep an inbox if the channel allows it."
)

// liveOnlyPublishNotice reads the publish result of a --retention none
// message. The daemon waits for the delivery to finish and reports how many
// online members were targeted, how many actually received it, and how many
// were listening but could not take it; an older daemon that reports no
// counts gets the general reminder.
func liveOnlyPublishNotice(response map[string]any) string {
	counts, reported := readDeliveryCounts(response)
	switch {
	case !reported:
		return "beam room: delivered live only (--retention none): members not listening right now will not receive this message.\n" + liveOnlyPublishHint
	case counts.Failed > 0:
		// An agent reports success when some member received the message
		// even though the delivery failed for another that was listening.
		return "beam room: " + lowerFirst(deliveryFailure(counts)) + "\n  hint: " + lowerFirst(deliveryFailureHint(counts))
	case counts.Delivered == 0:
		return "beam room: nobody received this message: no member was listening, and --retention none does not keep it for later.\n" + liveOnlyPublishHint
	case counts.missed() > 0:
		return fmt.Sprintf("beam room: delivered to %s; %s not listening and will never receive this message (--retention none).\n%s", memberCount(counts.Delivered), countedMembers(counts.missed()), liveOnlyPublishHint)
	}
	return ""
}

// lowerFirst lowercases the first letter of a sentence, which notices print
// after a "beam room:" or "hint:" prefix.
func lowerFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToLower(text[:1]) + text[1:]
}

func countedMembers(count int) string {
	if count == 1 {
		return "1 member was"
	}
	return fmt.Sprintf("%d members were", count)
}

// notice writes guidance for a person to the error stream. --json and --quiet
// are for programs, so they stay silent.
func (runner Runner) notice(mode outputMode, text string) {
	if mode.JSON || mode.Quiet {
		return
	}
	writer := runner.Notices
	if writer == nil {
		writer = runner.err()
	}
	_, _ = fmt.Fprintln(writer, text)
}

func (runner Runner) json(value any) error {
	encoder := json.NewEncoder(runner.out())
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func (runner Runner) flags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(runner.err())
	return flags
}

func (runner Runner) usage() {
	_, _ = fmt.Fprintln(runner.err(), "usage: beam room [--json|--quiet] <list|inspect|create|join|leave|close|refresh|invite|members|role|channel|grant> ...")
	_, _ = fmt.Fprintln(runner.err(), "       beam room channel object <publish|list|status|cancel> ROOM_ID CHANNEL_ID ...")
	_, _ = fmt.Fprintln(runner.err(), "       beam room channel media <publish|view|list> ROOM_ID CHANNEL_ID")
}

func (runner Runner) usageError(message string) error {
	if strings.TrimSpace(message) != "" {
		_, _ = fmt.Fprintln(runner.err(), message)
	}
	runner.usage()
	return errUsage
}

func (runner Runner) out() io.Writer {
	if runner.Out != nil {
		return runner.Out
	}
	return io.Discard
}

func (runner Runner) err() io.Writer {
	if runner.Err != nil {
		return runner.Err
	}
	return io.Discard
}

func (runner Runner) in() io.Reader {
	if runner.In != nil {
		return runner.In
	}
	return strings.NewReader("")
}

var errUsage = errors.New("usage")

func extractOutputMode(args []string) ([]string, outputMode) {
	result := make([]string, 0, len(args))
	mode := outputMode{}
	for _, arg := range args {
		switch arg {
		case "--json":
			mode.JSON = true
		case "--quiet", "-q":
			mode.Quiet = true
		default:
			result = append(result, arg)
		}
	}
	if mode.JSON {
		mode.Quiet = false
	}
	return result, mode
}

func requiredID(args []string, message string) (string, []string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", nil, errors.New(message)
	}
	return args[0], args[1:], nil
}

func roomPath(roomID string) string {
	return "/v1/rooms/" + url.PathEscape(roomID)
}

func parseActions(raw string) []btr.Action {
	seen := make(map[btr.Action]bool)
	var result []btr.Action
	for _, item := range strings.Split(raw, ",") {
		action := btr.Action(strings.TrimSpace(item))
		if action != "" && !seen[action] {
			seen[action] = true
			result = append(result, action)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func mapString(value map[string]any, objectKey, fieldKey string) string {
	object, _ := value[objectKey].(map[string]any)
	result, _ := object[fieldKey].(string)
	return result
}

func mapStringFlat(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func exitCode(err error) int {
	var daemonErr *tunnel.Error
	if errors.As(err, &daemonErr) {
		switch daemonErr.Kind {
		case tunnel.ErrUnavailable:
			return ExitDaemon
		case tunnel.ErrVersion:
			return ExitCompatibility
		case tunnel.ErrNotFound:
			return ExitNotFound
		case tunnel.ErrConflict:
			return ExitConflict
		}
		switch daemonErr.Status {
		case http.StatusUpgradeRequired, http.StatusNotImplemented:
			return ExitCompatibility
		case http.StatusNotFound, http.StatusGone:
			return ExitNotFound
		case http.StatusConflict:
			return ExitConflict
		case http.StatusUnauthorized, http.StatusForbidden:
			return ExitDenied
		default:
			return ExitFailed
		}
	}
	return ExitDaemon
}

func ParseUint(raw string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
}
