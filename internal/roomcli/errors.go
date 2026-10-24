package roomcli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// Failure describes a failed room command for a caller that renders errors
// itself. Message is empty for usage errors, whose text has already been
// written to the runner's error stream.
type Failure struct {
	Code    int
	Kind    string
	Message string
	Hint    string
}

// Error lets a command return a failure it detects itself, before any request
// reaches the agent; Execute reports it unchanged.
func (failure *Failure) Error() string { return failure.Message }

// explanation is what a known coordinator or daemon error means to the person
// running the command, and what they can do about it.
type explanation struct {
	// Kind is a stable identifier, surfaced as "kind" in --json errors.
	Kind string
	// Message replaces the raw error text when set.
	Message string
	Hint    string
	// DiagnoseChannel asks the runner to read the channel's local MLS
	// provisioning state, which tells a missing manager apart from a missing
	// grant when the daemon reports both the same way.
	DiagnoseChannel bool
	// DiagnoseDelivery asks the runner to read how many members a failed
	// live-only delivery reached, from the error or the workload status, so a
	// partial delivery is not reported as nobody receiving the message.
	DiagnoseDelivery bool
	// ShowReasons appends the typed reasons the error carries to Message, for
	// a failure whose cause is one of several stable reasons.
	ShowReasons bool
	ShowRefusal bool
}

// errorRule recognises one failure. A rule matches when any of its Codes
// equals the error's typed code (case-insensitively), or, for errors that do
// not carry a specific code yet, when the lowercased message contains any of
// its Phrases. Statuses, Commands, and Reasons narrow a match further.
//
// Typed codes are the contract; phrases are the fallback for failures that
// only carry prose today. To support a new coordinator code, add it to Codes
// of the rule it refines, or add a rule; once every supported daemon forwards
// the code, the phrase can go.
type errorRule struct {
	Codes    []string
	Phrases  []string
	Statuses []int
	// Commands lists room subcommands ("create", "channel publish", ...) the
	// rule applies to. Empty means every command.
	Commands []string
	// Reasons narrows a code to one of its reasons, read from the error's
	// details["reason"] or, failing that, from the message text.
	Reasons []string
	// TypedReasons match on their own: a daemon that forwards a coordinator
	// reason (details.reason or an entry of details.reasons, a top-level
	// reason, or a nested controller_unavailable object) may wrap it in a
	// generic code such as publish_failed or stream_failed.
	TypedReasons []string
	explanation
}

func (rule errorRule) matches(command string, err *tunnel.Error) bool {
	if len(rule.Commands) != 0 && !slices.Contains(rule.Commands, command) {
		return false
	}
	if len(rule.Statuses) != 0 && !slices.Contains(rule.Statuses, err.Status) {
		return false
	}
	if slices.ContainsFunc(errorReasons(err), func(typed string) bool {
		return slices.Contains(rule.TypedReasons, typed)
	}) {
		return true
	}
	reason := errorReason(err)
	detail := errorText(err)
	if len(rule.Reasons) != 0 && !slices.ContainsFunc(rule.Reasons, func(candidate string) bool {
		return reason == candidate || strings.Contains(detail, candidate)
	}) {
		return false
	}
	for _, code := range errorCodes(err) {
		for _, candidate := range rule.Codes {
			if code == strings.ToLower(candidate) {
				return true
			}
		}
	}
	for _, phrase := range rule.Phrases {
		if strings.Contains(detail, phrase) {
			return true
		}
	}
	return false
}

var nestedErrorKeys = []string{"controller_unavailable", "cause"}

// errorValue looks key up in the error's details, then in its other fields,
// then in a nested coordinator error object in either. The daemon's error
// shape is still settling, so every place it may put a value is accepted.
func errorValue(err *tunnel.Error, key string) any {
	sources := []map[string]any{err.Details, err.Fields}
	for _, source := range sources {
		if value, ok := source[key]; ok && value != nil {
			return value
		}
	}
	for _, source := range sources {
		for _, nestedKey := range nestedErrorKeys {
			if nested, ok := source[nestedKey].(map[string]any); ok && nested[key] != nil {
				return nested[key]
			}
		}
	}
	return nil
}

func errorString(err *tunnel.Error, key string) string {
	value, _ := errorValue(err, key).(string)
	return strings.TrimSpace(value)
}

func errorReason(err *tunnel.Error) string {
	return strings.ToLower(errorString(err, "reason"))
}

// errorReasons returns every typed reason the error carries, lowercased: its
// reason, then the entries of its reasons list, which the agent sends for a
// stream that failed or a datagram workload that ended before its source was
// read.
func errorReasons(err *tunnel.Error) []string {
	var reasons []string
	if reason := errorReason(err); reason != "" {
		reasons = append(reasons, reason)
	}
	list, _ := errorValue(err, "reasons").([]any)
	for _, value := range list {
		text, _ := value.(string)
		if reason := strings.ToLower(strings.TrimSpace(text)); reason != "" && !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

// errorCodes returns the error's own code and, when it wraps a coordinator
// error, the wrapped code, lowercased.
func errorCodes(err *tunnel.Error) []string {
	var codes []string
	for _, code := range []string{err.Code, errorString(err, "code")} {
		code = strings.ToLower(strings.TrimSpace(code))
		if code != "" && !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	return codes
}

// errorText is the lowercased message plus the text of a wrapped coordinator
// error, for phrase matching.
func errorText(err *tunnel.Error) string {
	text := err.Detail
	if wrapped := errorString(err, "error"); wrapped != "" && !strings.Contains(text, wrapped) {
		text += " " + wrapped
	}
	return strings.ToLower(text)
}

// Commands that move data on a channel, as named by roomCommandName.
var channelDataCommands = []string{
	"channel publish", "channel listen", "channel stream", "channel datagram", "channel object", "channel media",
}

const (
	// grantCommandsNote travels with every suggested grant command: a plain
	// member cannot run them, and the grants hide the channel from it
	// entirely when it lacks discover.
	grantCommandsNote  = "Only a Room owner or a member holding manage on the channel can run grant commands (other members get `BTR authorization denied` or a not-found channel), and `grant put` replaces the member's action list, so include the actions it already has."
	grantManageCommand = "`beam room ROOM_ID channel CHANNEL_ID grant put --subject-type member --subject MEMBER_ID --actions manage`"
	liveOnlyAdvice     = "With --retention none a message reaches only members running `listen` when it is published, and nothing is kept for later: start `listen` on the receiving side, then publish again, or use --retention sender_local if the channel allows it."
	workerRetryAdvice  = "This is on the Beam side and is usually temporary: retry in a moment. If it keeps happening, the network may have no Worker available for this right now; try again later."
	rekeyMessage       = "This channel is renewing its encryption keys after a membership change, so nothing was sent."
	rekeyHint          = "A member joined or left, or a webhook was accepted or moved, and every member gets the channel's new keys within seconds; the agent already waited for them. Retry in a moment. If it keeps failing for more than a minute, the agent of a member holding manage on the channel may be offline: check with `beam room ROOM_ID channel CHANNEL_ID show`."
)

// ownerGrantHint asks a Room owner to run a grant command for this member.
func ownerGrantHint(actions string) string {
	return "Ask a Room owner or a member holding manage on the channel to grant " + actions + " to this member or one of its roles: `beam room ROOM_ID channel CHANNEL_ID grant put --subject-type member --subject MEMBER_ID --actions " + actions + "` (member IDs: `beam room members ROOM_ID`). " + grantCommandsNote
}

// errorRules is ordered: the first matching rule wins, so specific rules come
// before generic ones.
var errorRules = []errorRule{
	// Room creation.
	{
		Commands: []string{"create"},
		Phrases:  []string{"requires an agent connected through a beam account"},
		explanation: explanation{
			Kind:    "room_create_requires_account_agent",
			Message: "This machine joined through a Room invitation, so it cannot create Rooms.",
			Hint:    "Only a machine connected to a Beam account can create Rooms. Run `beam agent disconnect`, then `beam agent connect` (with --api-key on a headless machine), and retry. Invitation-only machines can still join Rooms and use the channels they were granted.",
		},
	},
	{
		Commands: []string{"create"},
		Codes:    []string{"api_key_required"},
		explanation: explanation{
			Kind:    "room_api_key_required",
			Message: "Creating a Room requires a Beam API key.",
			Hint:    "Pass --api-key or set BEAM_API_KEY to a key whose role grants rooms:start.",
		},
	},
	{
		Codes: []string{"insufficient_credits"},
		explanation: explanation{
			Kind:        "room_insufficient_credits",
			Hint:        "The API key cannot pay for this. Add credits to the organization in the Beam Console, or raise the key's credit limit or monthly budget there, then retry.",
			ShowRefusal: true,
		},
	},
	{
		Codes: []string{"api_key_invalid"},
		explanation: explanation{
			Kind:    "room_api_key_invalid",
			Message: "Beam refused the API key: it is unknown, expired, revoked or disabled.",
			Hint:    "Renew or replace the key in the Beam Console, then pass it with --api-key or BEAM_API_KEY.",
		},
	},
	{
		Codes: []string{"permission_required"},
		explanation: explanation{
			Kind:        "room_permission_required",
			Hint:        "Use an API key whose role grants the permission this action needs, such as rooms:start to create a Room: pick or create one in the Beam Console and pass it with --api-key or BEAM_API_KEY.",
			ShowRefusal: true,
		},
	},
	{
		Codes: []string{"organization_blocked"},
		explanation: explanation{
			Kind: "room_organization_blocked",
			Hint: "The organization that owns the API key cannot run billable actions. An owner of the organization can contact Beam support.",
		},
	},
	{
		Commands: []string{"create"},
		Codes:    []string{"credit_unavailable"},
		explanation: explanation{
			Kind: "room_billing_unavailable",
			Hint: "Beam could not check the API key's credit. This is on the Beam side and is retryable; try again shortly.",
		},
	},

	// Room join. The coordinator's reason names why an invitation cannot be
	// used; the code and then the message stand in for it when it is absent.
	{Commands: joinCommands, TypedReasons: []string{"invitation_expired"}, explanation: invitationExpired},
	{Commands: joinCommands, TypedReasons: []string{"invitation_exhausted"}, explanation: invitationExhausted},
	{Commands: joinCommands, TypedReasons: []string{"invitation_revoked"}, explanation: invitationRevoked},
	{Commands: joinCommands, TypedReasons: []string{"invitation_invalid"}, explanation: invitationInvalid},
	{Commands: joinCommands, TypedReasons: []string{"invitation_not_found"}, explanation: invitationNotFound},
	{Commands: joinCommands, TypedReasons: []string{"room_closed"}, explanation: joinRoomClosed},
	{Commands: joinCommands, Codes: []string{"expired"}, Phrases: []string{"resource expired", "invitation expired", "invitation has expired"}, explanation: invitationExpired},
	{Commands: joinCommands, Phrases: []string{"consumed", "already used", "max_uses", "used up"}, explanation: invitationExhausted},
	{Commands: joinCommands, Phrases: []string{"invitation revoked", "invitation has been revoked", "invitation was revoked"}, explanation: invitationRevoked},
	{Commands: joinCommands, Codes: []string{"replayed"}, Phrases: []string{"no longer usable"}, explanation: invitationUnusable},
	{Commands: joinCommands, Phrases: []string{"room is closed"}, explanation: joinRoomClosed},
	{Commands: joinCommands, Codes: []string{"permission_denied"}, Phrases: []string{"authorization denied", "room_mismatch", "wrong room", "different room"}, explanation: invitationInvalid},
	{Commands: joinCommands, Codes: []string{"not_found"}, Phrases: []string{"resource not found"}, explanation: invitationNotFound},
	{Commands: joinCommands, Codes: []string{"resource_exhausted"}, explanation: joinLimited},

	// Room webhook members. The daemon answers these codes, and the coordinator's
	// refusals keep their status and code through it.
	{
		Codes:       []string{"room_webhooks_unavailable"},
		explanation: webhookManageUnavailable,
	},
	{
		Codes:       []string{"webhook_hosting_unavailable"},
		explanation: webhookHostUnavailable,
	},
	{
		Codes: []string{"webhook_host_ineligible"},
		explanation: explanation{
			Kind:    "webhook_host_requires_account_agent",
			Message: "This machine joined through a Room invitation only, so it cannot host webhooks.",
			Hint:    "Only an agent connected to a Beam account can host a webhook. Ask a Room owner to move it to a member whose agent is: " + webhookMoveCommand + ".",
		},
	},
	{
		Codes: []string{"webhook_controller_upgrade_required"},
		explanation: explanation{
			Kind: "webhook_controller_upgrade_required",
			Hint: "A webhook gets a channel's keys only from the member whose agent controls that channel, and the members named above run an agent released before webhook members. Ask them to upgrade beam (`beam update`), or take away their manage on the webhook's channels, then try again.",
		},
	},
	{
		Codes: []string{"webhook_channel_requires_retention"},
		explanation: explanation{
			Kind: "webhook_channel_requires_retention",
			Hint: "A webhook receives only live messages (retention none), so it would receive nothing on a channel that does not allow none. Allow none on that channel (`beam room ROOM_ID channel <channel-id> update --persistence-modes none,...`), or leave the channel out of the webhook's --channel grants.",
		},
	},
	{
		Commands: webhookHostCommands,
		Codes:    []string{"webhook_not_hosted", "not_found"},
		explanation: explanation{
			Kind:    "webhook_not_hosted",
			Message: "This machine's agent is not asked to host that webhook.",
			Hint:    "`beam webhook hosted` lists the webhooks this machine hosts or is asked to host. Accept, decline, rotate-secret and test run on the machine of the member named as host; a Room owner names it with `webhook add --host` or `webhook move`.",
		},
	},
	{
		Codes: []string{"webhook_not_pending"},
		explanation: explanation{
			Kind:    "webhook_not_pending",
			Message: "The webhook is not waiting for this machine to accept it.",
			Hint:    "It may already be accepted, moved to another host, or removed: check `beam webhook hosted`. To give an accepted webhook a new URL, a Room owner moves it (" + webhookMoveCommand + ") and the host accepts it again.",
		},
	},
	{
		Codes: []string{"webhook_not_accepted"},
		explanation: explanation{
			Kind:    "webhook_not_accepted",
			Message: "This machine has not accepted that webhook.",
			Hint:    "Accept it first: `beam room ROOM_ID webhook accept WEBHOOK_ID --url <https-url> --secret-file <path>`. Secret rotation and tests run on the machine that accepted it.",
		},
	},
	{
		Phrases: []string{"webhook host is not eligible"},
		explanation: explanation{
			Kind:    "webhook_host_ineligible",
			Message: "That member cannot host webhooks.",
			Hint:    "A host must be an active agent member of the Room whose agent is connected to a Beam account (not one that joined only through an invitation, and not a Studio-managed agent) and supports webhook hosting. Pick another member from `beam room ROOM_ID member list`. When accepting, update this machine with `beam update`.",
		},
	},
	{
		Phrases: []string{"webhook grants are limited"},
		explanation: explanation{
			Kind:    "webhook_grant_not_allowed",
			Message: "A webhook can hold only discover, subscribe and publish on message channels, and discover and subscribe on object channels.",
			Hint:    "Change the --channel grants: a webhook gets no manage, no roles, no other channel kind, and no publish on an object channel.",
		},
	},
	{
		Phrases: []string{"webhook state does not allow this change"},
		explanation: explanation{
			Kind:    "webhook_state_conflict",
			Message: "The webhook's current state does not allow that change.",
			Hint:    "Check it with `beam room ROOM_ID webhook show WEBHOOK_ID`. Resume works on a paused or degraded webhook that has an accepted host. A webhook whose host left or was moved needs a host first: " + webhookMoveCommand + ", then that host accepts it.",
		},
	},
	{
		Commands: webhookOwnerCommands,
		Codes:    []string{"resource_exhausted"},
		explanation: explanation{
			Kind:    "webhook_limit_reached",
			Message: "Webhook limit reached: a Room holds at most 10 webhooks, and one host agent at most 20 across Rooms, pending ones included.",
			Hint:    "Remove a webhook that is no longer needed (`beam room ROOM_ID webhook remove <member-id>`), or name another host.",
		},
	},
	{
		Commands: webhookOwnerCommands,
		Codes:    []string{"permission_denied"},
		explanation: explanation{
			Kind:    "webhook_permission_denied",
			Message: "Only a Room owner or admin can do that.",
			Hint:    "Adding, moving and removing webhooks needs the Room owner or admin role; the agent hosting a webhook can also resume it. Ask a Room owner.",
		},
	},
	{
		Commands: webhookHostCommands,
		Codes:    []string{"permission_denied"},
		explanation: explanation{
			Kind:    "webhook_host_permission_denied",
			Message: "This machine's agent is not the host of that webhook.",
			Hint:    "Only the agent of the member named as host can accept, decline or run a webhook. `beam webhook hosted` lists the ones this machine is asked to host.",
		},
	},
	{
		Commands: []string{"webhook add"},
		Codes:    []string{"not_found", "room_not_found"},
		explanation: explanation{
			Kind:    "webhook_target_not_found",
			Message: "The Room, the host member, or a granted channel was not found.",
			Hint:    "Check the host with `beam room ROOM_ID member list` and the channels with `beam room ROOM_ID channel list`.",
		},
	},
	{
		Commands: []string{"webhook move"},
		Codes:    []string{"not_found", "room_not_found"},
		explanation: explanation{
			Kind:    "webhook_target_not_found",
			Message: "The webhook or the new host member was not found.",
			Hint:    "List the webhooks with `beam room ROOM_ID webhook list` and the members with `beam room ROOM_ID member list`.",
		},
	},
	{
		Commands: webhookOwnerCommands,
		Codes:    []string{"not_found", "room_not_found"},
		explanation: explanation{
			Kind:    "webhook_not_found",
			Message: "The webhook was not found in this Room.",
			Hint:    "List the Room's webhooks with `beam room ROOM_ID webhook list`, and check the Room ID with `beam room list`.",
		},
	},
	{
		Commands: []string{"webhook accept", "webhook rotate-secret"},
		Codes:    []string{"invalid_request"},
		explanation: explanation{
			Kind: "webhook_invalid_request",
			Hint: "The URL must be an absolute https URL without user information or a fragment, and its host must resolve to a public address unless the webhook is accepted with --allow-private. The secret must be 16 to 512 bytes, and --overlap at most 7 days.",
		},
	},

	{
		Codes:        []string{"mls_delivery_controller_unavailable"},
		Phrases:      []string{"no eligible mls delivery controller"},
		Reasons:      []string{"no_manager_agent", "no member agent has manage"},
		TypedReasons: []string{"no_manager_agent"},
		explanation: explanation{
			Kind:    "mls_no_manager_agent",
			Message: "No member agent holds manage on this channel, so nobody can set up its encryption and messages cannot flow.",
			Hint:    "A Room owner must grant manage on the channel to a member agent: " + grantManageCommand + ". An organization owner or Studio member without an agent identity cannot act as controller. " + grantCommandsNote,
		},
	},
	{
		Codes:        []string{"mls_delivery_controller_unavailable"},
		Phrases:      []string{"no eligible mls delivery controller"},
		Reasons:      []string{"managers_offline", "with manage are offline"},
		TypedReasons: []string{"managers_offline"},
		explanation: explanation{
			Kind:    "mls_managers_offline",
			Message: "Every member agent holding manage on this channel is offline, so its encryption cannot progress and messages cannot flow.",
			Hint:    "Start the agent of a member holding manage (`beam agent start` on that machine), or ask a Room owner to grant manage to an online member agent: " + grantManageCommand + ". " + grantCommandsNote,
		},
	},
	{
		Codes:   []string{"mls_delivery_controller_unavailable"},
		Phrases: []string{"no eligible mls delivery controller", "did not match the elected controller"},
		explanation: explanation{
			Kind: "mls_controller_unavailable",
			Hint: "No online member agent holding manage on the channel can set up its encryption. Keep one online, or ask a Room owner to grant manage to a member agent: " + grantManageCommand + ". " + grantCommandsNote,
		},
	},

	// A restricted channel is invisible without discover, and the coordinator
	// reports it exactly like a channel that does not exist.
	{
		Commands: channelDataCommands,
		Codes:    []string{"not_found"},
		explanation: explanation{
			Kind:    "channel_not_visible",
			Message: channelNotVisibleMessage,
			Hint:    channelNotVisibleHint,
		},
	},
	{
		Commands: []string{"grant list", "grant put", "grant revoke"},
		Codes:    []string{"not_found"},
		explanation: explanation{
			Kind:    "channel_grants_not_visible",
			Message: "The channel or its grants are not visible to this member.",
			Hint:    "A channel's grants are visible only to a Room owner or a member holding manage on it; for everyone else the channel looks missing. Check the IDs with `beam room channel list ROOM_ID`, or ask a Room owner.",
		},
	},

	// A membership change is renewing the channel's MLS keys, and the agent's
	// bounded wait for the new epoch ran out.
	{
		Commands: channelDataCommands,
		Codes:    []string{"channel_rekeying"},
		explanation: explanation{
			Kind:            "channel_rekeying",
			Message:         rekeyMessage,
			Hint:            rekeyHint,
			DiagnoseChannel: true,
		},
	},
	// Agents without that wait report the epoch change itself.
	{
		Commands: channelDataCommands,
		Codes:    []string{"publish_failed"},
		Reasons:  []string{"key epoch mismatch"},
		explanation: explanation{
			Kind:            "channel_rekeying",
			Message:         rekeyMessage,
			Hint:            rekeyHint,
			DiagnoseChannel: true,
		},
	},

	// Channel encryption not ready for this member. The daemon reports a
	// hidden channel, a missing manager, a missing grant, and a not-yet-joined
	// group all as the same 400, so the runner reads the channel and its
	// provisioning state to tell them apart.
	{
		Commands: channelDataCommands,
		Phrases:  []string{"mls channel state is unavailable"},
		explanation: explanation{
			Kind:            "channel_not_ready",
			Message:         "This channel's encryption is not ready for this member, so nothing can be sent on it.",
			Hint:            "Likely causes: no online member agent holds manage on the channel (an organization owner or Studio member cannot act as controller), this member lacks the publish grant, or it joined moments ago. Retry in a moment; if it keeps failing, ask a Room owner to check the channel's grants.",
			DiagnoseChannel: true,
		},
	},
	{
		Commands: []string{"channel listen"},
		Codes:    []string{listenClosedCode},
		explanation: explanation{
			Kind:            "channel_listen_closed",
			Message:         "The listen stream ended before any message arrived.",
			Hint:            "Likely causes: this member lacks the subscribe grant on the channel, or its encryption is not ready because no online member agent holds manage. Ask a Room owner to check the channel's grants.",
			DiagnoseChannel: true,
		},
	},

	// Grants.
	{
		Commands: channelDataCommands,
		Codes:    []string{"permission_denied"},
		explanation: explanation{
			Kind:    "channel_permission_denied",
			Message: "This member is not allowed to do that on the channel.",
			Hint:    "Publishing needs the publish action and listening needs subscribe. " + ownerGrantHint("ACTION"),
		},
	},
	{
		Codes: []string{"permission_denied"},
		explanation: explanation{
			Kind: "room_permission_denied",
			Hint: "This member's roles and grants do not allow it. Changing channels, grants, roles, or invitations needs manage on the channel or the Room owner or admin role; ask a Room owner.",
		},
	},

	// Delivery with --retention none.
	{
		Commands: []string{"channel publish"},
		Codes:    []string{"no_online_subscriber"},
		explanation: explanation{
			Kind:    "channel_no_recipient",
			Message: "No other member allowed to receive this message is online, so it was not sent.",
			Hint:    "Receiving members must be connected (`beam agent status`). " + liveOnlyAdvice,
		},
	},
	{
		Commands: []string{"channel publish"},
		Codes:    []string{"no_authorized_subscriber"},
		explanation: explanation{
			Kind:    "channel_no_subscriber",
			Message: "No other member holds subscribe on this channel, so the message was not sent.",
			Hint:    ownerGrantHint("subscribe"),
		},
	},
	// No member confirmed receiving the message while some never answered:
	// they may have it, so it was neither reported lost nor sent again.
	{
		Commands: []string{"channel publish"},
		Codes:    []string{"delivery_unconfirmed"},
		explanation: explanation{
			Kind:             "channel_delivery_unconfirmed",
			Message:          "No member confirmed receiving this message, and some members' agents never answered, so they may have it.",
			Hint:             unconfirmedDeliveryHint,
			DiagnoseDelivery: true,
		},
	},
	{
		Commands: []string{"channel publish"},
		Codes:    []string{"room_workload_failed", "room_workload_expired"},
		explanation: explanation{
			Kind:             "channel_not_delivered",
			Message:          "The message was not delivered to every member, and --retention none does not keep it for the others.",
			Hint:             liveOnlyAdvice,
			DiagnoseDelivery: true,
		},
	},

	// Worker-served streams: the agent's own stream failures, then the
	// coordinator's refusals when a member agent lacks stream support or no
	// Worker offers it.
	{
		Commands: []string{"channel stream"},
		Codes:    []string{"stream_key_unavailable"},
		explanation: explanation{
			Kind:    "stream_key_unavailable",
			Message: "This machine could not get the channel's current encryption key, so the stream could not be sent or read.",
			Hint:    "Streams are end-to-end encrypted with the channel's key. Usually the key is rotating or this member's encryption setup is still finishing: retry in a moment. If it keeps failing, check that this member still belongs to the Room and that a member agent holding manage on the channel is online.",
		},
	},
	{
		Commands: []string{"channel stream"},
		Codes:    []string{"stream_renewal_failed"},
		explanation: explanation{
			Kind:        "stream_renewal_failed",
			Message:     "The stream stopped: Beam could not renew its Worker lease before it expired.",
			Hint:        "A stream runs on one-hour Worker leases that renew while it continues. This is on the Beam side and usually temporary: publish the stream again. worker_capability_unavailable means no Beam Worker offers streams right now; no_online_subscriber means no receiving member was still online.",
			ShowReasons: true,
		},
	},
	{
		Commands: []string{"channel stream"},
		Codes:    []string{"no_active_targets"},
		explanation: explanation{
			Kind:        "no_active_targets",
			Message:     "The stream stopped: every member receiving it dropped out before it ended.",
			Hint:        "Members receive a stream only if they run `stream listen` before it is published: start it on the receiving members, then publish again. no_live_subscriber or target_join_timeout means a member was not listening; target_heartbeat_timeout means its agent stopped responding.",
			ShowReasons: true,
		},
	},
	{
		Commands: []string{"channel stream"},
		Codes:    []string{"stream_truncated"},
		explanation: explanation{
			Kind:        "stream_truncated",
			Message:     "The stream ended without its end-of-stream marker, so the data received is incomplete.",
			Hint:        "source_aborted or source_lost means the publisher stopped or lost its connection; stream_gap means a Beam Worker could no longer serve the next part. Without a reason, this machine's agent may have stopped: check `beam agent status`. Start `stream listen` again before the stream is published again.",
			ShowReasons: true,
		},
	},
	{
		Commands: []string{"channel stream"},
		Codes:    []string{"agent_capability_unavailable"},
		Phrases:  []string{"workload-incapable", "no online target agent advertises"},
		Reasons:  []string{streamDirectCapability},
		explanation: explanation{
			Kind:    "agent_capability_unavailable",
			Message: "The stream could not start: a member's Beam agent does not support Worker-served streams (" + streamDirectCapability + ").",
			Hint:    "Update the Beam CLI and agent on every member's machine with `beam update`, make sure each agent is running (`beam agent status`), and retry.",
		},
	},
	{
		Commands: []string{"channel stream"},
		Codes:    []string{"worker_capability_unavailable"},
		Phrases:  []string{"no capable orchestrator", "no worker available"},
		explanation: explanation{
			Kind:    "worker_capability_unavailable",
			Message: "No Beam Worker offers streams right now, so the stream could not start.",
			Hint:    workerRetryAdvice,
		},
	},
	{
		Codes:   []string{"agent_capability_unavailable"},
		Phrases: []string{"workload-incapable", "no online target agent advertises"},
		explanation: explanation{
			Kind: "agent_capability_unavailable",
			Hint: "A member agent involved does not support this, or none is online. Run `beam update` on that machine, make sure its agent is running, and retry.",
		},
	},
	// A datagram workload the coordinator could not reserve a relay for fails
	// before anything is sent.
	{
		Commands:     []string{"channel datagram"},
		TypedReasons: []string{relayUnavailableReason},
		explanation: explanation{
			Kind:    "datagram_relay_unavailable",
			Message: "The datagram could not be sent: Beam has no relay available for it right now.",
			Hint:    "Datagram channels currently depend on Beam relays, and none is available, so nothing was sent. This is on the Beam side: retry later. For live audio or video, use a media channel instead: create one with --kind media, then run `beam room <room> channel <channel> media publish`. Media channels do not need a relay.",
		},
	},
	{
		Commands: channelDataCommands,
		Codes:    []string{"worker_capability_unavailable"},
		Phrases:  []string{"no capable orchestrator", "no worker available"},
		explanation: explanation{
			Kind:    "worker_capability_unavailable",
			Message: "No Beam Worker is available to deliver on this channel right now, so nothing was sent.",
			Hint:    workerRetryAdvice,
		},
	},
	{
		Codes:   []string{"worker_capability_unavailable"},
		Phrases: []string{"no capable orchestrator", "no worker available"},
		explanation: explanation{
			Kind: "worker_capability_unavailable",
			Hint: "No Beam Worker currently offers this capability. " + workerRetryAdvice,
		},
	},
	{
		Commands: []string{"channel publish"},
		Codes:    []string{"channel_setup_timeout"},
		Reasons:  workloadUnclaimedPhases,
		explanation: explanation{
			Kind:    "channel_setup_timeout",
			Message: "The message could not be delivered: no Beam delivery Worker picked it up in time.",
			Hint:    workerRetryAdvice + " Nothing was delivered, so publishing again does not duplicate it.",
		},
	},
	{
		Codes:   []string{"channel_setup_timeout"},
		Reasons: workloadUnclaimedPhases,
		explanation: explanation{
			Kind:    "channel_setup_timeout",
			Message: "The channel could not be set up: no Beam Worker picked the request up in time.",
			Hint:    workerRetryAdvice,
		},
	},
	{
		Codes: []string{"channel_setup_timeout"},
		explanation: explanation{
			Kind:    "channel_setup_timeout",
			Message: "The channel transfer started but did not finish in time.",
			Hint:    "A Beam Worker took the request, but the delivery stalled. Retry; if it keeps happening, `beam agent logs` shows the workload and its last state.",
		},
	},
}

// Webhook commands, as the command package names them for Explain. Owner
// commands manage a Room's webhooks; host commands run on the machine of the
// member named as a webhook's host.
var (
	webhookOwnerCommands = []string{"webhook add", "webhook list", "webhook show", "webhook remove", "webhook resume", "webhook move"}
	webhookHostCommands  = []string{"webhook hosted", "webhook accept", "webhook decline", "webhook rotate-secret", "webhook test"}
)

// joinCommands are the commands that join a Room with an invitation.
var joinCommands = []string{"join"}

const newInvitationHint = "Ask the Room owner for a new invitation."

// Why an invitation cannot be used to join a Room.
var (
	invitationExpired = explanation{
		Kind:    "invitation_expired",
		Message: "This Room invitation has expired.",
		Hint:    newInvitationHint,
	}
	invitationExhausted = explanation{
		Kind:    "invitation_exhausted",
		Message: "This Room invitation is used up: it has no uses left.",
		Hint:    newInvitationHint,
	}
	invitationRevoked = explanation{
		Kind:    "invitation_revoked",
		Message: "The Room owner revoked this invitation.",
		Hint:    newInvitationHint,
	}
	invitationUnusable = explanation{
		Kind:    "invitation_unusable",
		Message: "This Room invitation can no longer be used: it is used up or was revoked.",
		Hint:    newInvitationHint,
	}
	invitationInvalid = explanation{
		Kind:    "invitation_invalid",
		Message: "This invitation is not valid for this Room.",
		Hint:    "Check the Room ID and the invitation token. An invitation works only for its own Room, and only for the agent or account it names, if any.",
	}
	invitationNotFound = explanation{
		Kind:    "invitation_not_found",
		Message: "The invitation or its Room was not found.",
		Hint:    "Check the Room ID and the invitation token, or ask the Room owner for a new invitation.",
	}
	joinRoomClosed = explanation{
		Kind:    "room_closed",
		Message: "This Room is closed, so it cannot be joined.",
		Hint:    "Ask the Room owner for an invitation to an active Room.",
	}
	joinLimited = explanation{
		Kind:    "room_join_limited",
		Message: "The Room cannot take this join right now: it is full, or too many join attempts were made.",
		Hint:    "Wait a moment and try again. If it keeps failing, ask the Room owner.",
	}
)

// webhookMoveCommand names a webhook's new host. Hints fill in ROOM_ID and
// WEBHOOK_ID when the command knows them.
const webhookMoveCommand = "`beam room ROOM_ID webhook move WEBHOOK_ID --host <member-id>`"

var (
	webhookManageUnavailable = explanation{
		Kind:    "webhook_manage_unavailable",
		Message: "This machine's Beam agent cannot manage Room webhooks.",
		Hint:    "Webhook members need an agent that supports them and a coordinator that announces room-webhooks-v1. Update the CLI and agent together with `beam update`, then run `beam agent restart`. If the agent is already current, its coordinator does not support webhook members yet.",
	}
	webhookHostUnavailable = explanation{
		Kind:    "webhook_host_unavailable",
		Message: "This machine's Beam agent cannot host Room webhooks.",
		Hint:    "A host needs an agent that supports webhook hosting (`beam update`, then `beam agent restart`), connected to a Beam account with `beam agent connect` (a machine that joined only through a Room invitation cannot host), and a coordinator that supports webhook members. A Studio-managed agent cannot host. Otherwise ask a Room owner to move the webhook to another member: " + webhookMoveCommand + ".",
	}
)

// MissingWebhookCapability explains a local agent that does not advertise the
// webhook capability a command needs: btr.webhook.host for the host commands,
// btr.webhook.manage for the others.
func MissingWebhookCapability(host bool) Failure {
	known := webhookManageUnavailable
	if host {
		known = webhookHostUnavailable
	}
	return Failure{Code: ExitFailed, Kind: known.Kind, Message: known.Message, Hint: known.Hint}
}

// Explain describes the failure of a daemon call made by a room command that
// the command package runs itself, such as "webhook add". It reports false
// when no rule explains the failure.
func Explain(command string, err error) (Failure, bool) {
	known, ok := explainError(command, err)
	if !ok {
		return Failure{}, false
	}
	failure := Failure{Code: exitCode(err), Kind: known.Kind, Message: known.Message, Hint: known.Hint}
	if failure.Message == "" {
		failure.Message = err.Error()
	}
	return failure, true
}

// relayUnavailableReason is the reason a datagram workload fails with when the
// coordinator cannot reserve a relay for it.
const relayUnavailableReason = "room_workload_relay_unavailable"

// streamDirectCapability is the capability of Worker-served streams, which
// the coordinator names when a member agent lacks it.
const streamDirectCapability = "room.stream.direct.v1"

// workloadUnclaimedPhases are the TimeoutError phases in which no Worker had
// claimed the workload yet ("room workload provisioning timed out ...").
var workloadUnclaimedPhases = []string{"provisioning timed out", "submit timed out"}

const (
	channelNotVisibleMessage = "This member cannot see the channel: it does not exist in this Room, or it is restricted and this member lacks the discover grant on it."
	channelNotVisibleHint    = "Check the channel ID with `beam room channel list ROOM_ID`, which lists only the channels this member can see. On a restricted channel publish and subscribe are not enough: the member also needs discover. " +
		"Ask a Room owner or a member holding manage on the channel to add it: `beam room ROOM_ID channel CHANNEL_ID grant put --subject-type member --subject MEMBER_ID --actions discover,publish,subscribe`. " + grantCommandsNote
)

// listenClosedCode marks a listen stream that the daemon closed before any
// message arrived. The daemon does that without an error when this member may
// not subscribe or the channel is not ready, which used to exit 0 silently.
const listenClosedCode = "listen_stream_closed"

// explainError returns what err means for command, or false when it is not a
// failure the CLI knows how to explain.
func explainError(command string, err error) (explanation, bool) {
	var daemonErr *tunnel.Error
	if !errors.As(err, &daemonErr) {
		return explanation{}, false
	}
	if daemonErr.Kind == tunnel.ErrInterrupted {
		return interruptedExplanation(command), true
	}
	for _, rule := range errorRules {
		if rule.matches(command, daemonErr) {
			known := rule.explanation
			if known.ShowReasons {
				known.Message = withReasons(known.Message, errorReasons(daemonErr))
			}
			if known.ShowRefusal {
				message := known.Message
				if message == "" {
					message = daemonErr.Error()
				}
				known.Message = withRefusal(message, daemonErr)
			}
			return known, true
		}
	}
	// Only a failure no rule explains is read as a transport error, so a
	// typed code keeps its own meaning even when its message quotes one.
	if transport, ok := coordinatorTransportExplanation(daemonErr); ok {
		return transport, true
	}
	return explanation{}, false
}

// interruptedExplanation explains a request that connected to the agent and
// then got no answer. The agent may have acted on it, so the hint says how to
// check before running the command again rather than to retry.
func interruptedExplanation(command string) explanation {
	known := explanation{
		Kind:    "agent_request_interrupted",
		Message: "The agent stopped answering before it replied, so the request may have reached it.",
		Hint:    "Running the command again could repeat it. Check whether it took effect first, for example with `beam room list`, `beam room ROOM_ID show`, or the matching list command.",
	}
	if command == "channel object" {
		known.Hint = "If this was a publish, its publication may exist and be delivering: find it with `beam room ROOM_ID channel CHANNEL_ID object list` and follow it with `object watch PUBLICATION_ID` rather than publishing again, which would send the file a second time."
	}
	return known
}

// withReasons ends message with the reasons a failure carries, made safe to
// print on one line.
func withReasons(message string, reasons []string) string {
	if len(reasons) == 0 {
		return message
	}
	label := "reason"
	if len(reasons) > 1 {
		label = "reasons"
	}
	return strings.TrimSuffix(message, ".") + " (" + label + ": " + failureReason(strings.Join(reasons, ", ")) + ")."
}

func withRefusal(message string, err *tunnel.Error) string {
	var details []string
	required, hasRequired := errorValue(err, "credits_required").(float64)
	available, hasAvailable := errorValue(err, "credits_available").(float64)
	switch {
	case hasRequired && hasAvailable:
		details = append(details, "credits required: "+formatCredits(required)+", available: "+formatCredits(available))
	case hasRequired:
		details = append(details, "credits required: "+formatCredits(required))
	case hasAvailable:
		details = append(details, "credits available: "+formatCredits(available))
	}
	if reason := errorReason(err); reason != "" {
		details = append(details, "reason: "+failureReason(reason))
	}
	if permission := errorString(err, "permission"); permission != "" && !strings.Contains(message, permission) {
		details = append(details, "permission: "+failureReason(permission))
	}
	if len(details) == 0 {
		return message
	}
	return strings.TrimSuffix(message, ".") + " (" + strings.Join(details, "; ") + ")."
}

func formatCredits(credits float64) string {
	return strconv.FormatFloat(math.Round(credits*100)/100, 'f', -1, 64)
}

// transportErrorPattern finds a Go HTTP client error the daemon passed through
// as its message, such as
// `Post "https://coordinator/...": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`.
// The daemon may prefix it with its own context.
var transportErrorPattern = regexp.MustCompile(`(?i)\b(get|post|put|patch|delete|head) "(https?://[^"]*)": (.+)`)

// transportCause is one kind of transport failure, recognised by phrases in
// the Go error text after the request.
type transportCause struct {
	Timeout bool
	Label   string
	Phrases []string
}

// transportCauses is ordered: a timeout is reported as such even when the
// error text also mentions the connection.
var transportCauses = []transportCause{
	{Timeout: true, Label: "timed out", Phrases: []string{"client.timeout exceeded", "context deadline exceeded", "i/o timeout", "tls handshake timeout", "timeout awaiting response headers"}},
	{Label: "connection refused", Phrases: []string{"connection refused"}},
	{Label: "connection reset", Phrases: []string{"connection reset", "broken pipe"}},
	{Label: "host not found", Phrases: []string{"no such host"}},
	{Label: "network unreachable", Phrases: []string{"network is unreachable", "no route to host"}},
	{Label: "connection closed before a response", Phrases: []string{"eof"}},
}

const coordinatorTransportHint = "The request is retryable: run the command again in a moment. Check that this machine's agent is connected with `beam agent status`. If it keeps happening, check this machine's network access to the coordinator (firewall, proxy, or VPN)."

// coordinatorTransportExplanation explains a daemon error whose message is a
// raw HTTP client error from the daemon's request to the coordinator: a
// timeout, a refused or reset connection, or a connection closed early. The
// raw text is replaced, as it means little to a person and its URL may carry
// query parameters such as tokens.
func coordinatorTransportExplanation(err *tunnel.Error) (explanation, bool) {
	for _, text := range []string{err.Detail, errorString(err, "error")} {
		match := transportErrorPattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		cause := strings.ToLower(match[3])
		for _, candidate := range transportCauses {
			if !slices.ContainsFunc(candidate.Phrases, func(phrase string) bool { return strings.Contains(cause, phrase) }) {
				continue
			}
			request := strings.ToUpper(match[1]) + " " + redactedURL(match[2])
			if candidate.Timeout {
				return explanation{
					Kind:    "coordinator_timeout",
					Message: "The coordinator did not answer in time (" + request + ": " + candidate.Label + "). This is usually temporary and the request can be retried.",
					Hint:    coordinatorTransportHint,
				}, true
			}
			return explanation{
				Kind:    "coordinator_unreachable",
				Message: "The coordinator could not be reached (" + request + ": " + candidate.Label + "). This is usually temporary and the request can be retried.",
				Hint:    coordinatorTransportHint,
			}, true
		}
	}
	return explanation{}, false
}

// redactedURL keeps a URL's scheme, host, and path, dropping credentials, the
// query, and the fragment, which may carry tokens.
func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path, RawPath: parsed.RawPath}).String()
	}
	// An unparsable URL is cut to its scheme and host.
	scheme, rest, _ := strings.Cut(raw, "://")
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return scheme + "://" + rest
}

// roomCommandName names the room subcommand in args (after "tunnel room"),
// e.g. "create" or "channel publish", for matching errorRule.Commands.
func roomCommandName(args []string) string {
	if len(args) < 3 {
		return ""
	}
	switch args[2] {
	case "channel", "grant", "role":
		if len(args) >= 4 {
			return args[2] + " " + args[3]
		}
	}
	return args[2]
}

func (runner Runner) diagnoseChannel(ctx context.Context, args []string, known explanation) explanation {
	roomID, channelID, ok := publishOrListenTarget(args)
	if !ok {
		return known
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	channelPath := roomPath(roomID) + "/channels/" + url.PathEscape(channelID)
	var channel map[string]any
	if err := runner.Client.Do(ctx, http.MethodGet, channelPath, "", nil, &channel); err != nil {
		var daemonErr *tunnel.Error
		if errors.As(err, &daemonErr) && slices.Contains(errorCodes(daemonErr), "not_found") {
			return explanation{Kind: "channel_not_visible", Message: channelNotVisibleMessage, Hint: channelNotVisibleHint}
		}
	} else if controller, ok := controllerExplanation(channel, known); ok {
		return controller
	} else if channelRotating(channel) {
		return explanation{Kind: "channel_rekeying", Message: rekeyMessage, Hint: rekeyHint}
	}
	var state map[string]any
	if err := runner.Client.Do(ctx, http.MethodGet, channelPath+"/provisioning", "", nil, &state); err != nil {
		return known
	}
	if controller, ok := controllerExplanation(state, known); ok {
		return controller
	}
	action := channelAction(args)
	phase, _ := state["phase"].(string)
	// since is when the agent entered its current phase.
	duration := ""
	if raw, _ := state["since"].(string); raw != "" {
		if since, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			duration = " (for " + time.Since(since).Round(time.Second).String() + ")"
		}
	}
	switch phase {
	case "":
		return known
	case "rekeying":
		return explanation{Kind: "channel_rekeying", Message: rekeyMessage,
			Hint: "This machine's agent reports the channel re-keying" + duration + ". " + rekeyHint}
	case "active":
		// Encryption works on this machine and the channel is not rotating
		// (checked above), so what is left is the member's own grant for the
		// action it attempted. Current agents report that as permission_denied;
		// older ones only as an MLS state error.
		return explanation{
			Kind:    "channel_permission_denied",
			Message: fmt.Sprintf("This member is probably missing the %s grant on the channel (its encryption is active on this machine).", action),
			Hint:    ownerGrantHint(action),
		}
	}
	detail := fmt.Sprintf("This machine's agent reports the channel's encryption setup in phase %q", phase) + duration
	if lastError, _ := state["last_error"].(string); lastError != "" {
		detail += " (last error: " + lastError + ")"
	}
	known.Hint = detail + "; that phase can lag behind a join that already happened. Likely causes: no online member agent holds manage on the channel (an organization owner or Studio member cannot act as controller); this member joined moments ago and setup is still finishing; or this member lacks the " + action + " grant. " +
		"Retry in a moment. If it keeps failing, ask a Room owner to check the channel's grants and, if no member agent holds manage, to grant it: " + grantManageCommand + ". " + grantCommandsNote
	return known
}

// channelRotating reports a channel view, as the daemon returns it, whose key
// epoch must rotate: a member was removed or a webhook moved host, and no
// member can use the channel until the new epoch is active.
func channelRotating(view map[string]any) bool {
	if nested, ok := view["channel"].(map[string]any); ok {
		view = nested
	}
	rotating, _ := view["rotation_required"].(bool)
	return rotating
}

// controllerExplanation reads a typed controller failure from a channel or
// provisioning state the daemon returned: a code and reason at the top level,
// under details, or in a controller_unavailable object, or the text of
// last_error. Only MLS controller explanations are taken from state.
func controllerExplanation(state map[string]any, known explanation) (explanation, bool) {
	sources := []map[string]any{state}
	for _, key := range []string{"channel", "provisioning", "mls"} {
		if nested, ok := state[key].(map[string]any); ok {
			sources = append(sources, nested)
		}
	}
	for _, source := range sources {
		fields := source
		if details, ok := source["details"].(map[string]any); ok {
			fields = details
		}
		code, _ := source["last_error_code"].(string)
		detail, _ := source["last_error"].(string)
		synthetic := &tunnel.Error{Code: code, Detail: detail, Details: fields, Fields: source}
		if controller, ok := explainError("", synthetic); ok && strings.HasPrefix(controller.Kind, "mls_") {
			if controller.Message == "" {
				controller.Message = known.Message
			}
			return controller, true
		}
	}
	return explanation{}, false
}

// deliveryCounts is how the members a live-only message targeted fared.
type deliveryCounts struct {
	Delivered int
	// Failed members were listening, but the delivery to them failed, for
	// example because their agent could not decrypt the message.
	Failed int
	// NotListening members were online without a listener bound, so a
	// live-only message was never going to reach them.
	NotListening int
	// Pending members were never accounted for: still in flight when the
	// workload expired, or counted by a daemon that does not say why.
	Pending int
	// Unconfirmed members never answered: their agents neither took the
	// message nor said they were not listening, so they may have it.
	Unconfirmed int
	// DeliveredMembers, FailedMembers and UnconfirmedMembers name those
	// members, when known.
	DeliveredMembers   []string
	FailedMembers      []string
	UnconfirmedMembers []string
	// Reason is a short, sanitized cause of the failed deliveries, when the
	// daemon gives one.
	Reason string
}

// missed counts the members that did not receive the message without a
// failure: not listening, or never accounted for.
func (counts deliveryCounts) missed() int {
	return counts.NotListening + counts.Pending
}

// Keys daemons use, or are expected to use, for delivery counts. Each group
// is one outcome, and the largest count of a group is used because its keys
// overlap; the groups themselves do not.
var (
	deliveredCountKeys    = []string{"delivered_deliveries", "delivered"}
	failedCountKeys       = []string{"failed_deliveries", "failed"}
	notListeningCountKeys = []string{"not_listening", "not_listening_deliveries", "skipped_online_only", "no_live_subscriber"}
	pendingCountKeys      = []string{"pending_deliveries", "pending"}
	unconfirmedCountKeys  = []string{"expired_deliveries"}
	// undeliveredCountKeys count every member that did not receive the
	// message, whatever the reason.
	undeliveredCountKeys  = []string{"undelivered"}
	onlineCountKeys       = []string{"online_deliveries"}
	failedMemberKeys      = []string{"failed_member_ids", "failed_members"}
	deliveredMemberKeys   = []string{"delivered_member_ids"}
	unconfirmedMemberKeys = []string{"unconfirmed_member_ids"}
	failureReasonKeys     = []string{"failure_reason", "reason"}
)

// Destination statuses of a room workload. A member reported unavailable,
// dropped, or failed was not listening when its agent says so; otherwise its
// delivery failed. Any other status is still in flight.
var (
	deliveredStatuses    = []string{"completed", "delivered"}
	notListeningStatuses = []string{"skipped_online_only", "not_listening"}
	missStatuses         = []string{"failed", "unavailable", "dropped"}
	notListeningReasons  = []string{"no_live_subscriber", "not_listening", "expired"}
)

// notListeningReason reports whether a destination's failure reason means the
// member had no listener: the reason token current agents send, or the error
// text older agents send instead ("... has no live subscriber").
func notListeningReason(reason string) bool {
	reason = strings.ToLower(reason)
	return slices.Contains(notListeningReasons, reason) || strings.Contains(reason, "no live subscriber")
}

// readDeliveryCounts finds delivery counts in a publish result, an error body,
// or a workload status, including nested workload, details, or counts
// objects and a workload's per-destination statuses.
func readDeliveryCounts(sources ...map[string]any) (deliveryCounts, bool) {
	for _, source := range sources {
		if counts, ok := deliveryCountsIn(source); ok {
			return counts, true
		}
	}
	for _, source := range sources {
		for _, key := range []string{"workload", "delivery", "counts", "details"} {
			if nested, ok := source[key].(map[string]any); ok {
				if counts, ok := readDeliveryCounts(nested); ok {
					return counts, true
				}
			}
		}
	}
	return deliveryCounts{}, false
}

func deliveryCountsIn(source map[string]any) (deliveryCounts, bool) {
	delivered, hasDelivered := firstCount(source, deliveredCountKeys)
	online, hasOnline := firstCount(source, onlineCountKeys)
	if !hasDelivered && !hasOnline {
		return destinationCounts(source)
	}
	failed, _ := maxCount(source, failedCountKeys)
	notListening, hasNotListening := maxCount(source, notListeningCountKeys)
	pending, hasPending := maxCount(source, pendingCountKeys)
	unconfirmed, hasUnconfirmed := maxCount(source, unconfirmedCountKeys)
	counts := deliveryCounts{Delivered: delivered, Failed: failed, NotListening: notListening, Pending: pending,
		Unconfirmed: unconfirmed}
	undelivered, hasUndelivered := maxCount(source, undeliveredCountKeys)
	if undelivered > counts.Failed+counts.Unconfirmed+counts.missed() {
		counts.Pending = undelivered - counts.Failed - counts.Unconfirmed - counts.NotListening
	}
	switch {
	case !hasDelivered:
		counts.Delivered = max(online-counts.Failed-counts.Unconfirmed-counts.missed(), 0)
	case hasOnline && !hasNotListening && !hasPending && !hasUnconfirmed && !hasUndelivered:
		// A daemon that reports only the online and delivered counts (and
		// perhaps the failed one) leaves the rest unaccounted for.
		counts.Pending = max(online-counts.Delivered-counts.Failed, 0)
	}
	counts.DeliveredMembers = stringList(source, deliveredMemberKeys)
	counts.FailedMembers = stringList(source, failedMemberKeys)
	counts.UnconfirmedMembers = stringList(source, unconfirmedMemberKeys)
	counts.Reason = failureReason(firstString(source, failureReasonKeys))
	return counts.withMemberFailures(source), true
}

// withMemberFailures names failed and unconfirmed members from a result's
// member_failures ({member_id, reason}), which a successful publish result
// carries instead of the error's member lists.
func (counts deliveryCounts) withMemberFailures(source map[string]any) deliveryCounts {
	failures, _ := source["member_failures"].([]any)
	var failed, unconfirmed []string
	reason := ""
	for _, raw := range failures {
		failure, _ := raw.(map[string]any)
		member := firstString(failure, []string{"member_id"})
		cause := firstString(failure, []string{"reason"})
		switch {
		case member == "":
		case cause == deliveryReasonUnconfirmed:
			unconfirmed = append(unconfirmed, member)
		default:
			failed = append(failed, member)
			if reason == "" {
				reason = cause
			}
		}
	}
	if len(counts.FailedMembers) == 0 {
		counts.FailedMembers = failed
	}
	if len(counts.UnconfirmedMembers) == 0 {
		counts.UnconfirmedMembers = unconfirmed
	}
	if counts.Reason == "" || counts.Reason == deliveryReasonUnconfirmed {
		counts.Reason = failureReason(reason)
	}
	return counts
}

// destinationCounts counts a workload's per-destination statuses.
func destinationCounts(source map[string]any) (deliveryCounts, bool) {
	destinations, ok := source["destinations"].([]any)
	if !ok || len(destinations) == 0 {
		return deliveryCounts{}, false
	}
	var counts deliveryCounts
	var reasons []string
	for _, raw := range destinations {
		destination, _ := raw.(map[string]any)
		status := strings.ToLower(firstString(destination, []string{"status", "state"}))
		reason := firstString(destination, []string{"reason", "error_code"})
		switch {
		case slices.Contains(deliveredStatuses, status):
			counts.Delivered++
		case slices.Contains(notListeningStatuses, status),
			slices.Contains(missStatuses, status) && notListeningReason(reason):
			counts.NotListening++
		case slices.Contains(missStatuses, status):
			counts.Failed++
			if member := firstString(destination, []string{"target_member_id", "member_id", "recipient_member_id"}); member != "" {
				counts.FailedMembers = append(counts.FailedMembers, member)
			}
			if reason != "" && !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		default:
			counts.Pending++
		}
	}
	counts.Reason = failureReason(strings.Join(reasons, "; "))
	return counts, true
}

func firstCount(source map[string]any, keys []string) (int, bool) {
	for _, key := range keys {
		if count, ok := countValue(source[key]); ok {
			return count, true
		}
	}
	return 0, false
}

func maxCount(source map[string]any, keys []string) (int, bool) {
	best, found := 0, false
	for _, key := range keys {
		if count, ok := countValue(source[key]); ok {
			best, found = max(best, count), true
		}
	}
	return best, found
}

func countValue(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), number >= 0
	case int:
		return number, number >= 0
	}
	return 0, false
}

func firstString(source map[string]any, keys []string) string {
	for _, key := range keys {
		if value, _ := source[key].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringList(source map[string]any, keys []string) []string {
	for _, key := range keys {
		values, ok := source[key].([]any)
		if !ok {
			continue
		}
		var list []string
		for _, value := range values {
			if text, _ := value.(string); strings.TrimSpace(text) != "" {
				list = append(list, strings.TrimSpace(text))
			}
		}
		if len(list) != 0 {
			return list
		}
	}
	return nil
}

// maxFailureReasonLength bounds the failure reason shown to a person; the
// full cause is in the receiving member's agent logs.
const maxFailureReasonLength = 160

// singleLine makes daemon-supplied text safe to print on one line: control
// characters and runs of whitespace become single spaces.
func singleLine(text string) string {
	return strings.Join(strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// failureReason makes a daemon-supplied failure reason safe to print on one
// line and cuts a long reason short.
func failureReason(reason string) string {
	reason = singleLine(reason)
	if runes := []rune(reason); len(runes) > maxFailureReasonLength {
		reason = strings.TrimSpace(string(runes[:maxFailureReasonLength-3])) + "..."
	}
	return reason
}

// withErrorFailureDetails fills in the failed members and failure reason from
// the error itself when the counts were read elsewhere, such as from the
// workload status or a nested object.
func withErrorFailureDetails(counts deliveryCounts, err *tunnel.Error) deliveryCounts {
	if counts.Failed == 0 {
		return counts
	}
	for _, source := range []map[string]any{err.Details, err.Fields} {
		if len(counts.FailedMembers) == 0 {
			counts.FailedMembers = stringList(source, failedMemberKeys)
		}
		if counts.Reason == "" {
			counts.Reason = failureReason(firstString(source, failureReasonKeys))
		}
	}
	return counts
}

// workloadIDPattern reads the workload ID from the daemon's terminal workload
// error text, "room workload ended in state expired (room=... workload=ID)".
var workloadIDPattern = regexp.MustCompile(`workload=([A-Za-z0-9_.:-]+)`)

const notListeningAdvice = "Only members running `listen` when a message is published receive it. Start `listen` on the other members before publishing, or use --retention sender_local if the channel allows it."

// failedDeliveryAdvice follows every report of a failed delivery. A live-only
// publish prints it as a notice, so it names the running binary itself.
func failedDeliveryAdvice() string {
	return "If it keeps failing, run `" + version.CLIName() + " agent logs` on the receiving member's machine to see why its agent could not accept the message."
}

// Delivery reasons the agent reports as stable tokens for a member that may
// well have been listening.
const (
	// deliveryReasonUnconfirmed: the member's agent never answered.
	deliveryReasonUnconfirmed = "delivery_unconfirmed"
	// deliveryReasonStaleScope: the channel re-keyed while the message was in
	// flight, so the member could not open it.
	deliveryReasonStaleScope = "stale_scope"
)

// deliveryReasonText words a delivery reason token for a person; other
// reasons are shown as the agent gives them.
func deliveryReasonText(reason string) string {
	if reason == deliveryReasonStaleScope {
		return "the channel was re-keyed while the message was in flight"
	}
	return reason
}

// unconfirmedDeliveryHint follows a report of members that never answered.
const unconfirmedDeliveryHint = "Their agents did not answer in time, for example because the channel was re-keyed while the message was in flight, or because an agent is busy or restarting. Publishing again can deliver the message twice to members that did receive it. If it keeps happening, check that their agents are running (`beam agent status` on their machines)."

// deliveryFailure words a live-only publish in which the delivery failed for
// at least one member that was listening, or at least one member never
// answered, naming them.
func deliveryFailure(counts deliveryCounts) string {
	var sentences []string
	if counts.Delivered == 0 && counts.Unconfirmed > 0 {
		sentences = append(sentences, "No member confirmed receiving this message: "+unconfirmedMembers(counts)+
			" did not answer, so they may have it.")
	}
	if counts.Failed > 0 {
		text := "Delivery failed for " + listeningMembers(counts.Failed)
		if len(counts.FailedMembers) != 0 {
			text += " (" + memberList(counts.FailedMembers) + ")"
		}
		if counts.Reason != "" {
			text += ": " + deliveryReasonText(counts.Reason)
		}
		sentences = append(sentences, text+".")
	}
	if counts.Delivered > 0 && counts.Unconfirmed > 0 {
		sentences = append(sentences, unconfirmedMembers(counts)+" did not confirm it, so they may have it.")
	}
	missed := counts.missed()
	switch {
	case counts.Delivered > 0 && missed > 0:
		sentences = append(sentences, fmt.Sprintf("It was delivered to %s; %s not listening and will never get it (--retention none).",
			deliveredMembers(counts), countedMembers(missed)))
	case counts.Delivered > 0:
		sentences = append(sentences, "It was delivered to "+deliveredMembers(counts)+".")
	case counts.Unconfirmed > 0 && missed > 0:
		sentences = append(sentences, fmt.Sprintf("%s not listening.", countedMembers(missed)))
	case counts.Unconfirmed > 0:
	case missed > 0:
		sentences = append(sentences, fmt.Sprintf("No member received it; %s not listening, and --retention none does not keep it for later.", countedMembers(missed)))
	default:
		sentences = append(sentences, "No member received it.")
	}
	return strings.Join(sentences, " ")
}

// deliveredMembers counts the members that received the message and names
// them when known.
func deliveredMembers(counts deliveryCounts) string {
	text := memberCount(counts.Delivered)
	if len(counts.DeliveredMembers) != 0 {
		text += " (" + memberList(counts.DeliveredMembers) + ")"
	}
	return text
}

// unconfirmedMembers counts the members that never answered and names them
// when known.
func unconfirmedMembers(counts deliveryCounts) string {
	text := memberCount(counts.Unconfirmed)
	if len(counts.UnconfirmedMembers) != 0 {
		text += " (" + memberList(counts.UnconfirmedMembers) + ")"
	}
	return text
}

// deliveryFailureExplanation is the kind, message and hint of a live-only
// publish whose delivery failed or went unconfirmed for some member and that
// no member is known to have received. Members that never answered may have
// it; a re-key that every member refused is named as such.
func deliveryFailureExplanation(counts deliveryCounts) (string, string, string) {
	switch {
	case counts.Delivered == 0 && counts.Unconfirmed > 0:
		return "channel_delivery_unconfirmed", deliveryFailure(counts), unconfirmedDeliveryHint
	case counts.Delivered == 0 && counts.Reason == deliveryReasonStaleScope:
		return "channel_rekeying", deliveryFailure(counts), rekeyHint
	}
	return "channel_delivery_failed", deliveryFailure(counts), deliveryFailureHint(counts)
}

// deliveryFailureHint says what to do about failed deliveries: publishing
// again retries them, and the receiving agent's logs say why they failed.
func deliveryFailureHint(counts deliveryCounts) string {
	hint := "The failure may be temporary: publish again to retry"
	if counts.Delivered > 0 || counts.Unconfirmed > 0 {
		hint += " (members that already received the message get it again)"
	}
	hint += ". " + failedDeliveryAdvice()
	if counts.missed() > 0 {
		hint += " Members that were not listening need `listen` running before you publish."
	}
	return hint
}

// diagnoseDelivery rewords a failed live-only publish from the delivery
// counts in the error or, failing that, in the workload status. An expired
// workload can still have reached the members that were listening, and a
// failed one can have failed for a member that was listening.
func (runner Runner) diagnoseDelivery(ctx context.Context, args []string, err error, known explanation) explanation {
	var daemonErr *tunnel.Error
	if !errors.As(err, &daemonErr) {
		return known
	}
	counts, ok := readDeliveryCounts(daemonErr.Details, daemonErr.Fields)
	if !ok {
		counts, ok = runner.workloadDeliveryCounts(ctx, args, daemonErr)
	}
	if !ok {
		return known
	}
	counts = withErrorFailureDetails(counts, daemonErr)
	switch {
	case counts.Failed > 0 || counts.Unconfirmed > 0:
		known.Kind, known.Message, known.Hint = deliveryFailureExplanation(counts)
	case counts.Delivered == 0 && counts.missed() > 0:
		known.Message = "No member received this message: none was listening when it was published, and --retention none does not keep it for later."
	case counts.Delivered == 0:
		// Counts that account for nobody say nothing about why.
	case counts.missed() > 0:
		known.Kind = "channel_partially_delivered"
		known.Message = fmt.Sprintf("Delivered to %s; %s not listening and will never get it (--retention none).", memberCount(counts.Delivered), countedMembers(counts.missed()))
		known.Hint = notListeningAdvice
	default:
		known.Kind = "channel_partially_delivered"
		known.Message = fmt.Sprintf("Delivered to %s, but the delivery did not complete for every targeted member (--retention none).", memberCount(counts.Delivered))
		known.Hint = notListeningAdvice
	}
	return known
}

func (runner Runner) workloadDeliveryCounts(ctx context.Context, args []string, err *tunnel.Error) (deliveryCounts, bool) {
	roomID, channelID, ok := publishOrListenTarget(args)
	if !ok {
		return deliveryCounts{}, false
	}
	workloadID := errorString(err, "workload_id")
	if workloadID == "" {
		if match := workloadIDPattern.FindStringSubmatch(err.Detail); match != nil {
			workloadID = match[1]
		}
	}
	if workloadID == "" {
		return deliveryCounts{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var status map[string]any
	path := roomPath(roomID) + "/channels/" + url.PathEscape(channelID) + "/workloads/" + url.PathEscape(workloadID)
	if err := runner.Client.Do(ctx, http.MethodGet, path, "", nil, &status); err != nil {
		return deliveryCounts{}, false
	}
	return readDeliveryCounts(status)
}

// listeningMembers counts members that were listening.
func listeningMembers(count int) string {
	if count == 1 {
		return "1 listening member"
	}
	return fmt.Sprintf("%d listening members", count)
}

// maxListedMembers bounds how many member IDs a message names.
const maxListedMembers = 3

// memberList names members, eliding all but the first few.
func memberList(members []string) string {
	if len(members) <= maxListedMembers {
		return strings.Join(members, ", ")
	}
	return strings.Join(members[:maxListedMembers], ", ") + fmt.Sprintf(" and %d more", len(members)-maxListedMembers)
}

func memberCount(count int) string {
	if count == 1 {
		return "1 member"
	}
	return fmt.Sprintf("%d members", count)
}

// publishOrListenTarget returns the room and channel of a publish or listen
// command (tunnel room channel <publish|listen> ROOM_ID CHANNEL_ID ...).
func publishOrListenTarget(args []string) (string, string, bool) {
	command := roomCommandName(args)
	if (command != "channel publish" && command != "channel listen") || len(args) < 6 {
		return "", "", false
	}
	return args[4], args[5], true
}

// channelAction is the grant a publish or listen command needs.
func channelAction(args []string) string {
	if roomCommandName(args) == "channel listen" {
		return "subscribe"
	}
	return "publish"
}

// withTarget fills the ROOM_ID and CHANNEL_ID placeholders of a hint with the
// IDs the command was given, and ACTION with the grant it needs, so the
// suggested command can be run as is.
func withTarget(hint string, args []string) string {
	roomID, channelID, ok := publishOrListenTarget(args)
	if !ok && strings.HasPrefix(roomCommandName(args), "grant ") && len(args) >= 6 {
		// tunnel room grant <list|put|revoke> ROOM_ID CHANNEL_ID ...
		roomID, channelID, ok = args[4], args[5], true
	}
	if !ok && roomCommandName(args) == "channel object" && len(args) >= 7 && args[4] == "publish" {
		// tunnel room channel object publish ROOM_ID CHANNEL_ID ...
		roomID, channelID, ok = args[5], args[6], true
	}
	if ok {
		hint = strings.ReplaceAll(hint, "ROOM_ID", roomID)
		hint = strings.ReplaceAll(hint, "CHANNEL_ID", channelID)
		hint = strings.ReplaceAll(hint, "ACTION", channelAction(args))
	}
	return hint
}
