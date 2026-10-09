package roomcli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
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
	// reason (details.reason, a top-level reason, or a nested
	// controller_unavailable object) may wrap it in a generic code such as
	// publish_failed.
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
	reason := errorReason(err)
	if reason != "" && slices.Contains(rule.TypedReasons, reason) {
		return true
	}
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
		Phrases:  []string{"missing the rooms:start permission", "not allowed to run billable actions"},
		explanation: explanation{
			Kind:    "room_api_key_permission",
			Message: "The Beam API key is not allowed to start Rooms (it lacks the rooms:start permission).",
			Hint:    "Creating a Room is billable and needs an API key whose role grants rooms:start. Pick or create such a key in the Beam Console and pass it with --api-key or BEAM_API_KEY.",
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
		Commands: []string{"create"},
		Codes:    []string{"credit_required"},
		explanation: explanation{
			Kind: "room_credit_required",
			Hint: "The API key cannot pay for a Room right now. Check the organization's credit and the key's credit limit and monthly budget in the Beam Console, or use another key whose role grants rooms:start.",
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

	{
		Codes:   []string{"agent_capability_unavailable"},
		Phrases: []string{"workload-incapable", "no online target agent advertises"},
		explanation: explanation{
			Kind: "agent_capability_unavailable",
			Hint: "A member agent involved does not support this, or none is online. Run `beam update` on that machine, make sure its agent is running, and retry.",
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
	for _, rule := range errorRules {
		if rule.matches(command, daemonErr) {
			return rule.explanation, true
		}
	}
	// Only a failure no rule explains is read as a transport error, so a
	// typed code keeps its own meaning even when its message quotes one.
	if transport, ok := coordinatorTransportExplanation(daemonErr); ok {
		return transport, true
	}
	return explanation{}, false
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
	switch phase {
	case "":
		return known
	case "active":
		// Encryption works on this machine, so what is left is the member's own
		// grant for the action it attempted.
		return explanation{
			Kind:    "channel_permission_denied",
			Message: fmt.Sprintf("This member is probably missing the %s grant on the channel (its encryption is active on this machine).", action),
			Hint:    ownerGrantHint(action),
		}
	}
	detail := fmt.Sprintf("This machine's agent reports the channel's encryption setup in phase %q", phase)
	if raw, _ := state["since"].(string); raw != "" {
		if since, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			detail += " (for " + time.Since(since).Round(time.Second).String() + ")"
		}
	}
	if lastError, _ := state["last_error"].(string); lastError != "" {
		detail += " (last error: " + lastError + ")"
	}
	known.Hint = detail + "; that phase can lag behind a join that already happened. Likely causes: no online member agent holds manage on the channel (an organization owner or Studio member cannot act as controller); this member joined moments ago and setup is still finishing; or this member lacks the " + action + " grant. " +
		"Retry in a moment. If it keeps failing, ask a Room owner to check the channel's grants and, if no member agent holds manage, to grant it: " + grantManageCommand + ". " + grantCommandsNote
	return known
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
	// workload expired, or counted by a daemon that does not say why. For a
	// live-only message that almost always means they were not listening, as
	// the agent itself counts an expired delivery.
	Pending int
	// FailedMembers names the members whose delivery failed, when known.
	FailedMembers []string
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
	pendingCountKeys      = []string{"pending_deliveries", "pending", "expired_deliveries"}
	// undeliveredCountKeys count every member that did not receive the
	// message, whatever the reason.
	undeliveredCountKeys = []string{"undelivered"}
	onlineCountKeys      = []string{"online_deliveries"}
	failedMemberKeys     = []string{"failed_member_ids", "failed_members"}
	failureReasonKeys    = []string{"failure_reason", "reason"}
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
	counts := deliveryCounts{Delivered: delivered, Failed: failed, NotListening: notListening, Pending: pending}
	undelivered, hasUndelivered := maxCount(source, undeliveredCountKeys)
	if undelivered > counts.Failed+counts.missed() {
		counts.Pending = undelivered - counts.Failed - counts.NotListening
	}
	switch {
	case !hasDelivered:
		counts.Delivered = max(online-counts.Failed-counts.missed(), 0)
	case hasOnline && !hasNotListening && !hasPending && !hasUndelivered:
		// A daemon that reports only the online and delivered counts (and
		// perhaps the failed one) leaves the rest unaccounted for.
		counts.Pending = max(online-counts.Delivered-counts.Failed, 0)
	}
	counts.FailedMembers = stringList(source, failedMemberKeys)
	counts.Reason = failureReason(firstString(source, failureReasonKeys))
	return counts, true
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

const (
	notListeningAdvice = "Only members running `listen` when a message is published receive it. Start `listen` on the other members before publishing, or use --retention sender_local if the channel allows it."
	// failedDeliveryAdvice follows every report of a failed delivery.
	failedDeliveryAdvice = "If it keeps failing, run `beam agent logs` on the receiving member's machine to see why its agent could not accept the message."
)

// deliveryFailure words a live-only publish in which the delivery failed for
// at least one listening member.
func deliveryFailure(counts deliveryCounts) string {
	text := "Delivery failed for " + listeningMembers(counts.Failed)
	if len(counts.FailedMembers) != 0 {
		text += " (" + memberList(counts.FailedMembers) + ")"
	}
	if counts.Reason != "" {
		text += ": " + counts.Reason
	}
	text += "."
	missed := counts.missed()
	switch {
	case counts.Delivered > 0 && missed > 0:
		text += fmt.Sprintf(" It was delivered to %s; %s not listening and will never get it (--retention none).", memberCount(counts.Delivered), countedMembers(missed))
	case counts.Delivered > 0:
		text += fmt.Sprintf(" It was delivered to %s.", memberCount(counts.Delivered))
	case missed > 0:
		text += fmt.Sprintf(" No member received it; %s not listening, and --retention none does not keep it for later.", countedMembers(missed))
	default:
		text += " No member received it."
	}
	return text
}

// deliveryFailureHint says what to do about failed deliveries: publishing
// again retries them, and the receiving agent's logs say why they failed.
func deliveryFailureHint(counts deliveryCounts) string {
	hint := "The failure may be temporary: publish again to retry"
	if counts.Delivered > 0 {
		hint += " (members that already received the message get it again)"
	}
	hint += ". " + failedDeliveryAdvice
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
	case counts.Failed > 0:
		known.Kind = "channel_delivery_failed"
		known.Message = deliveryFailure(counts)
		known.Hint = deliveryFailureHint(counts)
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
	if ok {
		hint = strings.ReplaceAll(hint, "ROOM_ID", roomID)
		hint = strings.ReplaceAll(hint, "CHANNEL_ID", channelID)
		hint = strings.ReplaceAll(hint, "ACTION", channelAction(args))
	}
	return hint
}
