package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// Local agent capabilities behind the webhook routes: managing a Room's
// webhooks, hosting the ones this machine is named for, and delivering them
// durably.
const (
	webhookManageCapability  = "btr.webhook.manage"
	webhookHostCapability    = "btr.webhook.host"
	webhookDurableCapability = "btr.webhook.host.durable"
	webhookAsyncCapability   = "btr.webhook.host.async"
)

const (
	webhookMaxFileMiB = 25
	// A webhook signing secret is 16 to 512 bytes. Input is read up to a
	// bound well above that, so an oversized file is reported, not truncated.
	webhookSecretMinBytes  = 16
	webhookSecretMaxBytes  = 512
	webhookSecretReadLimit = 64 << 10
	webhookMaxOverlap      = 7 * 24 * time.Hour
)

// webhookActions are the channel actions a webhook may hold.
var webhookActions = []string{"discover", "subscribe", "publish"}

// webhookView is the daemon's webhook view. It never carries the webhook URL
// or its signing secret. Presence is the webhook member's own presence: it is
// online, and publishes are routed to it, while its host's agent runs it and
// the host is online. HostPresence is the host member's presence, online or
// offline, and empty without a host. Local is present on the host routes
// once this machine accepted the webhook. Durable is the durable setting the
// host reported, nil from a coordinator without durable delivery.
// UpgradeRequiredMemberIDs names the
// members that may control a channel the webhook uses and run an agent
// without webhook support; the webhook's state does not show them.
type webhookView struct {
	MemberID       string            `json:"member_id"`
	RoomID         string            `json:"room_id"`
	HostMemberID   string            `json:"host_member_id"`
	HostAgentID    string            `json:"host_agent_id"`
	Presence       string            `json:"presence"`
	HostPresence   string            `json:"host_presence"`
	State          string            `json:"state"`
	Reason         string            `json:"reason"`
	URLFingerprint string            `json:"url_fingerprint"`
	HostName       string            `json:"host_name"`
	PayloadMode    string            `json:"payload_mode"`
	MaxFileBytes   uint64            `json:"max_file_bytes"`
	Durable        *bool             `json:"durable"`
	Version        uint64            `json:"version"`
	Local          *webhookLocalView `json:"local"`

	UpgradeRequiredMemberIDs []string `json:"upgrade_required_member_ids"`
	KeysPendingChannelIDs    []string `json:"keys_pending_channel_ids"`
}

// webhookLocalView is the part of a hosted webhook's delivery state on this
// machine that the human output shows; --json prints all of it.
type webhookLocalView struct {
	Running             bool   `json:"running"`
	PreviousSecretUntil string `json:"previous_secret_until"`
	Health              struct {
		ConsecutiveFailures int `json:"consecutive_failures"`
	} `json:"health"`
	CircuitOpen       bool              `json:"circuit_open"`
	Durable           bool              `json:"durable"`
	DurableChannelIDs []string          `json:"durable_channel_ids"`
	Spool             *webhookSpoolView `json:"spool"`
	AsyncReplies      int               `json:"async_replies"`
}

// webhookSpoolView is a durable webhook's spool on this machine.
type webhookSpoolView struct {
	Pending     int    `json:"pending"`
	DeadLetters int    `json:"dead_letters"`
	Bytes       uint64 `json:"bytes"`
	ShareBytes  uint64 `json:"share_bytes"`
}

// webhookDeadLetter is an event a durable webhook's host stopped retrying.
type webhookDeadLetter struct {
	PublicationID string `json:"publication_id"`
	ChannelID     string `json:"channel_id"`
	Event         string `json:"event"`
	ReceivedAt    string `json:"received_at"`
	Attempts      int    `json:"attempts"`
	HTTPStatus    int    `json:"http_status"`
	Reason        string `json:"reason"`
	KeepUntil     string `json:"keep_until"`
}

type webhookTestView struct {
	Class        string `json:"class"`
	HTTPStatus   int    `json:"http_status"`
	Attempts     int    `json:"attempts"`
	Reason       string `json:"reason"`
	Reply        bool   `json:"reply"`
	AsyncReply   bool   `json:"async_reply"`
	ReplyIgnored string `json:"reply_ignored"`
}

// webhookGrant is one --channel grant of a new webhook.
type webhookGrant struct {
	ChannelID string   `json:"channel_id"`
	Actions   []string `json:"actions"`
}

// roomWebhookCommand runs "beam room ROOM webhook ...". Owner and admin
// commands call the daemon's /v1/rooms/{room}/webhooks routes, and host
// commands its /v1/hosted-webhooks routes.
func (a *App) roomWebhookCommand(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	action, rest := args[0], args[1:]
	switch action {
	case "add":
		return roomWebhookAdd(ctx, roomID, rest, client, renderer)
	case "list":
		if len(rest) != 0 {
			return usage("room webhook list does not accept arguments")
		}
		return roomWebhookList(ctx, roomID, client, renderer)
	case "show", "remove", "resume":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return usage("room webhook " + action + " requires one webhook member ID")
		}
		return roomWebhookMemberAction(ctx, action, roomID, rest[0], client, renderer)
	case "move":
		return roomWebhookMove(ctx, roomID, rest, client, renderer)
	case "accept":
		return a.roomWebhookAccept(ctx, roomID, rest, client, renderer)
	case "decline":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return usage("room webhook decline requires one webhook member ID")
		}
		return roomWebhookDecline(ctx, roomID, rest[0], client, renderer)
	case "rotate-secret":
		return a.roomWebhookRotateSecret(ctx, roomID, rest, client, renderer)
	case "test":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return usage("room webhook test requires one webhook member ID")
		}
		return roomWebhookTest(ctx, roomID, rest[0], client, renderer)
	case "durable":
		if len(rest) != 2 || strings.HasPrefix(rest[0], "-") || (rest[1] != "on" && rest[1] != "off") {
			return usage("room webhook durable requires one webhook member ID and on or off")
		}
		return roomWebhookDurable(ctx, roomID, rest[0], rest[1] == "on", client, renderer)
	case "reply-window":
		if len(rest) != 2 || strings.HasPrefix(rest[0], "-") {
			return usage("room webhook reply-window requires one webhook member ID and a duration")
		}
		seconds, err := webhookReplyWindow(rest[1])
		if err != nil {
			return err
		}
		return roomWebhookReplyWindow(ctx, roomID, rest[0], seconds, client, renderer)
	case "dead-letters":
		return roomWebhookDeadLetters(ctx, roomID, rest, client, renderer)
	default:
		return usage(fmt.Sprintf("unknown room webhook command %q", action))
	}
}

// webhookCommand runs "beam webhook ...", the webhooks this machine hosts
// across Rooms.
func (a *App) webhookCommand(ctx context.Context, args []string, cfg config.Config, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" {
		return renderNamedHelp("webhook", renderer)
	}
	switch args[0] {
	case "hosted":
		if len(args) != 1 {
			return usage("webhook hosted does not accept arguments")
		}
		client, err := a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		return listHostedWebhooks(ctx, client, renderer)
	default:
		return usage(fmt.Sprintf("unknown webhook command %q", args[0]))
	}
}

func roomWebhookAdd(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--name": true, "--host": true, "--channel": true, "--payload-mode": true, "--max-file-mb": true, "--idempotency-key": true}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	if positionals, err := positionalsForOptions(args, valueFlags, nil); err != nil {
		return err
	} else if len(positionals) != 0 {
		return usage("room webhook add takes no positional arguments")
	}
	name, err := requiredFlag(args, "--name")
	if err != nil {
		return err
	}
	host, err := requiredFlag(args, "--host")
	if err != nil {
		return err
	}
	grants, err := parseWebhookGrants(flagValues(args, "--channel"))
	if err != nil {
		return err
	}
	body := map[string]any{"display_name": strings.TrimSpace(name), "host_member_id": strings.TrimSpace(host), "grants": grants}
	if err := addWebhookDeliveryOptions(args, body); err != nil {
		return err
	}
	key, _, err := flag(args, "--idempotency-key")
	if err != nil {
		return err
	}
	if key == "" {
		key = newWebhookIdempotencyKey("add")
	}
	if err := requireWebhookCapability(ctx, client, webhookManageCapability, roomID, ""); err != nil {
		return err
	}
	var response struct {
		Webhook webhookView `json:"webhook"`
	}
	raw, err := callWebhook(ctx, client, http.MethodPost, roomWebhooksPath(roomID), key, body, &response)
	if err != nil {
		return webhookError("webhook add", roomID, "", err)
	}
	webhook := response.Webhook
	if renderer.Mode == output.Quiet {
		_, err := fmt.Fprintln(renderer.Out, webhook.MemberID)
		return err
	}
	return renderer.Result(raw, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Webhook %s added to Room %s. It stays pending until host member %s accepts it.\n"+
			"On the host's machine: %s room %s webhook accept %s --url <https-url> --secret-file <path>\n",
			webhook.MemberID, roomID, webhook.HostMemberID, version.CLIName(), roomID, webhook.MemberID)
		return err
	})
}

func roomWebhookList(ctx context.Context, roomID string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if err := requireWebhookCapability(ctx, client, webhookManageCapability, roomID, ""); err != nil {
		return err
	}
	var response struct {
		Webhooks []webhookView `json:"webhooks"`
	}
	raw, err := callWebhook(ctx, client, http.MethodGet, roomWebhooksPath(roomID), "", nil, &response)
	if err != nil {
		return webhookError("webhook list", roomID, "", err)
	}
	if len(response.Webhooks) == 0 {
		renderer.Progress("This Room has no webhooks. Add one with `" + version.CLIName() + " room " + roomID + " webhook add`.")
	}
	return renderer.Result(raw, func(w io.Writer) error {
		rows := make([][]string, 0, len(response.Webhooks))
		for _, webhook := range response.Webhooks {
			rows = append(rows, []string{
				webhook.MemberID, webhook.State, orDash(webhook.Reason), webhookDelivery(webhook), orDash(webhook.HostMemberID),
				orDash(webhook.HostName), webhook.PayloadMode, formatMiB(webhook.MaxFileBytes),
			})
		}
		if err := renderer.Table(w, output.Table{
			Headers: []string{"MEMBER ID", "STATE", "REASON", "DELIVERY", "HOST MEMBER", "URL HOST", "PAYLOAD", "MAX FILE"},
			Rows:    rows,
			Style:   output.RowStyle(1),
		}); err != nil {
			return err
		}
		return writeWebhooksBlocked(w, response.Webhooks)
	})
}

// roomWebhookMemberAction shows, resumes, or removes one webhook.
func roomWebhookMemberAction(ctx context.Context, action, roomID, memberID string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if err := requireWebhookCapability(ctx, client, webhookManageCapability, roomID, memberID); err != nil {
		return err
	}
	path := roomWebhooksPath(roomID) + "/" + url.PathEscape(memberID)
	command := "webhook " + action
	if action == "remove" {
		raw, err := callWebhook(ctx, client, http.MethodDelete, path, newWebhookIdempotencyKey(action), nil, nil)
		if err != nil {
			return webhookError(command, roomID, memberID, err)
		}
		return renderer.Result(raw, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Webhook %s removed from Room %s.\n", memberID, roomID)
			return err
		})
	}
	method, key := http.MethodGet, ""
	if action == "resume" {
		method, path, key = http.MethodPost, path+"/resume", newWebhookIdempotencyKey(action)
	}
	var response struct {
		Webhook webhookView `json:"webhook"`
	}
	raw, err := callWebhook(ctx, client, method, path, key, nil, &response)
	if err != nil {
		return webhookError(command, roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error { return writeWebhookDetails(w, response.Webhook) })
}

func roomWebhookMove(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--host": true}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, nil)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("room webhook move requires one webhook member ID")
	}
	memberID := positionals[0]
	host, err := requiredFlag(args, "--host")
	if err != nil {
		return err
	}
	if err := requireWebhookCapability(ctx, client, webhookManageCapability, roomID, memberID); err != nil {
		return err
	}
	var response struct {
		Webhook webhookView `json:"webhook"`
	}
	path := roomWebhooksPath(roomID) + "/" + url.PathEscape(memberID) + "/host"
	raw, err := callWebhook(ctx, client, http.MethodPost, path, newWebhookIdempotencyKey("move"),
		map[string]any{"host_member_id": strings.TrimSpace(host)}, &response)
	if err != nil {
		return webhookError("webhook move", roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Webhook %s now waits for host member %s, which must accept it with the URL and secret again.\n"+
			"On the new host's machine: %s room %s webhook accept %s --url <https-url> --secret-file <path>\n",
			memberID, response.Webhook.HostMemberID, version.CLIName(), roomID, memberID)
		return err
	})
}

// webhookTrustModel is shown before this machine accepts a webhook.
const webhookTrustModel = `Accepting makes this machine the webhook's host:
  - it decrypts everything sent to the webhook and POSTs it to the URL;
  - it can publish in the Room as the webhook (the receiver's replies);
  - the URL and signing secret stay on this machine only; Beam sees the URL's host name and a fingerprint;
  - delivery is live only unless the webhook is durable: nothing reaches it while this machine's agent is offline.
    A durable webhook keeps the events of channels that allow receiver_local on this machine, encrypted,
    and retries them for up to 24 hours.`

func (a *App) roomWebhookAccept(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--url": true, "--secret-file": true, "--payload-mode": true, "--max-file-mb": true,
		"--reply-window": true}
	boolFlags := map[string]bool{"--secret-stdin": true, "--allow-private": true, "--yes": true, "--durable": true}
	if err := rejectInlineSecret(args); err != nil {
		return err
	}
	if err := validateOptions(args, valueFlags, boolFlags); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, boolFlags)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("room webhook accept requires one webhook member ID")
	}
	durable := has(args, "--durable")
	memberID := positionals[0]
	rawURL, err := requiredFlag(args, "--url")
	if err != nil {
		return err
	}
	if err := validateWebhookURL(rawURL); err != nil {
		return err
	}
	secretFile, fromStdin, err := webhookSecretSource(args)
	if err != nil {
		return err
	}
	body := map[string]any{"url": rawURL, "allow_private": has(args, "--allow-private")}
	if durable {
		body["durable"] = true
	}
	if err := addWebhookDeliveryOptions(args, body); err != nil {
		return err
	}
	windowValue, replyWindow, err := flag(args, "--reply-window")
	if err != nil {
		return err
	}
	if replyWindow {
		seconds, err := webhookReplyWindow(windowValue)
		if err != nil {
			return err
		}
		body["async_reply_window_seconds"] = seconds
	}
	var secret string
	if secretFile != "" {
		if secret, err = readWebhookSecretFile(secretFile); err != nil {
			return err
		}
	}
	if err := requireWebhookCapability(ctx, client, webhookHostCapability, roomID, memberID); err != nil {
		return err
	}
	if durable {
		if err := requireWebhookCapability(ctx, client, webhookDurableCapability, roomID, memberID); err != nil {
			return err
		}
	}
	if replyWindow {
		if err := requireWebhookCapability(ctx, client, webhookAsyncCapability, roomID, memberID); err != nil {
			return err
		}
	}
	if err := a.confirmWebhookTrust(renderer, roomID, memberID, has(args, "--yes")); err != nil {
		return err
	}
	if fromStdin {
		if secret, err = a.readWebhookSecretStdin(renderer); err != nil {
			return err
		}
	}
	body["secret"] = secret
	var response struct {
		Webhook webhookView `json:"webhook"`
	}
	raw, err := callWebhook(ctx, client, http.MethodPost, hostedWebhookPath(roomID, memberID)+"/accept", "", body, &response)
	if err != nil {
		return webhookError("webhook accept", roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error {
		if _, err := fmt.Fprintf(w, "Webhook %s accepted: this machine now delivers it to %s.\n", memberID,
			orDash(response.Webhook.HostName)); err != nil {
			return err
		}
		if durable {
			if _, err := fmt.Fprintln(w, webhookDurableNote(response.Webhook.Local)); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "Send a signed test event with: %s room %s webhook test %s\n", version.CLIName(), roomID, memberID)
		return err
	})
}

// roomWebhookDurable turns durable delivery of a hosted webhook on or off.
func roomWebhookDurable(ctx context.Context, roomID, memberID string, durable bool, client *tunnel.LocalClient, renderer output.Renderer) error {
	if err := requireWebhookCapability(ctx, client, webhookDurableCapability, roomID, memberID); err != nil {
		return err
	}
	var response struct {
		Webhook webhookView `json:"webhook"`
	}
	raw, err := callWebhook(ctx, client, http.MethodPatch, hostedWebhookPath(roomID, memberID), "",
		map[string]any{"durable": durable}, &response)
	if err != nil {
		return webhookError("webhook durable", roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error {
		text := "Webhook " + memberID + " now delivers live only; the events already kept on this machine are still delivered."
		if durable {
			text = "Webhook " + memberID + " now delivers durably. " + webhookDurableNote(response.Webhook.Local)
		}
		_, err := fmt.Fprintln(w, text)
		return err
	})
}

// roomWebhookReplyWindow sets how long the host polls for an asynchronous
// reply.
func roomWebhookReplyWindow(ctx context.Context, roomID, memberID string, seconds int64, client *tunnel.LocalClient,
	renderer output.Renderer) error {
	if err := requireWebhookCapability(ctx, client, webhookAsyncCapability, roomID, memberID); err != nil {
		return err
	}
	raw, err := callWebhook(ctx, client, http.MethodPatch, hostedWebhookPath(roomID, memberID), "",
		map[string]any{"async_reply_window_seconds": seconds}, nil)
	if err != nil {
		return webhookError("webhook reply-window", roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Webhook %s: this machine now polls for an asynchronous reply for up to %s. "+
			"Replies already polled for keep their window.\n", memberID, time.Duration(seconds)*time.Second)
		return err
	})
}

// webhookReplyWindow reads an asynchronous reply window, 1m to 24h, in
// seconds.
func webhookReplyWindow(raw string) (int64, error) {
	window, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || window < time.Minute || window > 24*time.Hour || window%time.Second != 0 {
		return 0, usage("the reply window must be a duration from 1m to 24h, such as 2h")
	}
	return int64(window / time.Second), nil
}

// webhookDurableNote says which events a durable webhook keeps. The agent
// knows the webhook's channels only once the webhook runs.
func webhookDurableNote(local *webhookLocalView) string {
	switch {
	case local == nil || !local.Running:
		return "Once the webhook runs, `" + version.CLIName() + " webhook hosted` shows the channels whose events this machine keeps."
	case len(local.DurableChannelIDs) == 0:
		return "No channel of this webhook allows receiver_local retention yet, so its events stay live until one does."
	}
	return fmt.Sprintf("Events of %s are kept on this machine, encrypted, and retried for up to 24 hours.",
		webhookChannelCount(local.DurableChannelIDs))
}

// roomWebhookDeadLetters lists, replays or purges the dead letters of a
// durable webhook hosted on this machine.
func roomWebhookDeadLetters(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if len(args) == 0 {
		return usage("room webhook dead-letters requires list, replay, or purge")
	}
	action, rest := args[0], args[1:]
	if action != "list" && action != "replay" && action != "purge" {
		return usage(fmt.Sprintf("unknown room webhook dead-letters command %q", action))
	}
	boolFlags := map[string]bool{}
	if action != "list" {
		boolFlags["--all"] = true
	}
	if err := validateOptions(rest, map[string]bool{}, boolFlags); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(rest, map[string]bool{}, boolFlags)
	if err != nil {
		return err
	}
	if len(positionals) == 0 {
		return usage("room webhook dead-letters " + action + " requires one webhook member ID")
	}
	memberID, publicationIDs := positionals[0], positionals[1:]
	all := has(rest, "--all")
	switch {
	case action == "list" && len(publicationIDs) != 0:
		return usage("room webhook dead-letters list requires one webhook member ID")
	case action != "list" && all == (len(publicationIDs) != 0):
		return usage("room webhook dead-letters " + action + " requires publication IDs or --all")
	}
	if err := requireWebhookCapability(ctx, client, webhookDurableCapability, roomID, memberID); err != nil {
		return err
	}
	path := hostedWebhookPath(roomID, memberID) + "/dead-letters"
	if action == "list" {
		var response struct {
			DeadLetters []webhookDeadLetter `json:"dead_letters"`
		}
		raw, err := callWebhook(ctx, client, http.MethodGet, path, "", nil, &response)
		if err != nil {
			return webhookError("webhook dead-letters", roomID, memberID, err)
		}
		if len(response.DeadLetters) == 0 {
			renderer.Progress("Webhook " + memberID + " has no dead letters.")
		}
		return renderer.Result(raw, func(w io.Writer) error {
			rows := make([][]string, 0, len(response.DeadLetters))
			for _, letter := range response.DeadLetters {
				status := "-"
				if letter.HTTPStatus != 0 {
					status = strconv.Itoa(letter.HTTPStatus)
				}
				rows = append(rows, []string{letter.PublicationID, letter.ChannelID, letter.Event, letter.ReceivedAt,
					strconv.Itoa(letter.Attempts), status, orDash(letter.Reason), letter.KeepUntil})
			}
			return renderer.Table(w, output.Table{
				Headers: []string{"PUBLICATION ID", "CHANNEL ID", "EVENT", "RECEIVED", "ATTEMPTS", "HTTP", "REASON", "KEPT UNTIL"},
				Rows:    rows,
			})
		})
	}
	body := map[string]any{}
	if all {
		body["all"] = true
	} else {
		body["publication_ids"] = publicationIDs
	}
	var response struct {
		Replayed int `json:"replayed"`
		Purged   int `json:"purged"`
	}
	raw, err := callWebhook(ctx, client, http.MethodPost, path+"/"+action, "", body, &response)
	if err != nil {
		return webhookError("webhook dead-letters", roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error {
		again := "they are delivered again with their original Idempotency-Key"
		if response.Replayed == 1 {
			again = "it is delivered again with its original Idempotency-Key"
		}
		text := fmt.Sprintf("%s of webhook %s replayed; %s.", webhookDeadLetterCount(response.Replayed), memberID, again)
		if action == "purge" {
			text = fmt.Sprintf("%s of webhook %s deleted.", webhookDeadLetterCount(response.Purged), memberID)
		}
		_, err := fmt.Fprintln(w, text)
		return err
	})
}

func webhookDeadLetterCount(count int) string {
	if count == 1 {
		return "1 dead letter"
	}
	return fmt.Sprintf("%d dead letters", count)
}

func roomWebhookDecline(ctx context.Context, roomID, memberID string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if err := requireWebhookCapability(ctx, client, webhookHostCapability, roomID, memberID); err != nil {
		return err
	}
	raw, err := callWebhook(ctx, client, http.MethodPost, hostedWebhookPath(roomID, memberID)+"/decline", "", nil, nil)
	if err != nil {
		return webhookError("webhook decline", roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Webhook %s declined; it is removed from Room %s.\n", memberID, roomID)
		return err
	})
}

func (a *App) roomWebhookRotateSecret(ctx context.Context, roomID string, args []string, client *tunnel.LocalClient, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--secret-file": true, "--overlap": true}
	boolFlags := map[string]bool{"--secret-stdin": true}
	if err := rejectInlineSecret(args); err != nil {
		return err
	}
	if err := validateOptions(args, valueFlags, boolFlags); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, boolFlags)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("room webhook rotate-secret requires one webhook member ID")
	}
	memberID := positionals[0]
	secretFile, fromStdin, err := webhookSecretSource(args)
	if err != nil {
		return err
	}
	body := map[string]any{}
	if raw, ok, flagErr := flag(args, "--overlap"); flagErr != nil {
		return flagErr
	} else if ok {
		overlap, parseErr := time.ParseDuration(raw)
		if raw == "0" {
			overlap, parseErr = 0, nil
		}
		if parseErr != nil || overlap < 0 || overlap > webhookMaxOverlap {
			return usage("--overlap must be a duration from 0 to 168h, such as 24h")
		}
		body["overlap_seconds"] = int64(overlap / time.Second)
	}
	if err := requireWebhookCapability(ctx, client, webhookHostCapability, roomID, memberID); err != nil {
		return err
	}
	var secret string
	if fromStdin {
		secret, err = a.readWebhookSecretStdin(renderer)
	} else {
		secret, err = readWebhookSecretFile(secretFile)
	}
	if err != nil {
		return err
	}
	body["secret"] = secret
	var response struct {
		Webhook webhookView `json:"webhook"`
	}
	raw, err := callWebhook(ctx, client, http.MethodPost, hostedWebhookPath(roomID, memberID)+"/secret", "", body, &response)
	if err != nil {
		return webhookError("webhook rotate-secret", roomID, memberID, err)
	}
	return renderer.Result(raw, func(w io.Writer) error {
		message := fmt.Sprintf("Signing secret of webhook %s rotated.", memberID)
		if local := response.Webhook.Local; local != nil && local.PreviousSecretUntil != "" {
			message += " Requests also carry a signature with the previous secret until " + local.PreviousSecretUntil + "."
		} else {
			message += " The previous secret no longer signs requests."
		}
		_, err := fmt.Fprintln(w, message)
		return err
	})
}

func roomWebhookTest(ctx context.Context, roomID, memberID string, client *tunnel.LocalClient, renderer output.Renderer) error {
	if err := requireWebhookCapability(ctx, client, webhookHostCapability, roomID, memberID); err != nil {
		return err
	}
	var response struct {
		Test webhookTestView `json:"test"`
	}
	raw, err := callWebhook(ctx, client, http.MethodPost, hostedWebhookPath(roomID, memberID)+"/test", "", nil, &response)
	if err != nil {
		return webhookError("webhook test", roomID, memberID, err)
	}
	result := response.Test
	if err := renderer.Result(raw, func(w io.Writer) error {
		text := "Test delivery: " + result.Class + " (" + webhookAttempts(result) + ")."
		switch {
		case result.Reply:
			text += " The response is a valid reply; for a real delivery it would be published as the webhook."
		case result.AsyncReply:
			text += " The response starts an asynchronous reply; for a real delivery this machine would poll its Location" +
				" and publish the reply as the webhook."
		case result.ReplyIgnored != "":
			text += " The response is not a reply: " + result.ReplyIgnored + "."
		}
		_, err := fmt.Fprintln(w, text)
		return err
	}); err != nil {
		return err
	}
	if result.Class == "delivered" {
		return nil
	}
	return typedCLIError(ExitOperationFailed, "webhook_test_failed",
		"The test delivery failed: "+result.Class+" ("+webhookAttempts(result)+").", webhookTestHint(roomID, memberID, result.Class), nil)
}

func listHostedWebhooks(ctx context.Context, client *tunnel.LocalClient, renderer output.Renderer) error {
	if err := requireWebhookCapability(ctx, client, webhookHostCapability, "", ""); err != nil {
		return err
	}
	var response struct {
		Webhooks []webhookView `json:"webhooks"`
	}
	raw, err := callWebhook(ctx, client, http.MethodGet, "/v1/hosted-webhooks", "", nil, &response)
	if err != nil {
		return webhookError("webhook hosted", "", "", err)
	}
	pending := 0
	for _, webhook := range response.Webhooks {
		if webhook.State == "pending" {
			pending++
		}
	}
	switch {
	case len(response.Webhooks) == 0:
		renderer.Progress("This machine hosts no webhooks and is not asked to host any.")
	case pending > 0:
		renderer.Progress("Accept a pending webhook with: " + version.CLIName() + " room <room-id> webhook accept <member-id> --url <https-url> --secret-file <path>")
	}
	return renderer.Result(raw, func(w io.Writer) error {
		rows := make([][]string, 0, len(response.Webhooks))
		for _, webhook := range response.Webhooks {
			rows = append(rows, []string{
				webhook.RoomID, webhook.MemberID, webhook.State, orDash(webhook.Reason),
				orDash(webhook.HostName), hostedDelivery(webhook.Local),
			})
		}
		if err := renderer.Table(w, output.Table{
			Headers: []string{"ROOM ID", "MEMBER ID", "STATE", "REASON", "URL HOST", "DELIVERY"},
			Rows:    rows,
			Style:   output.RowStyle(2),
		}); err != nil {
			return err
		}
		return writeWebhooksBlocked(w, response.Webhooks)
	})
}

// callWebhook calls a daemon webhook route. It returns the response as the
// daemon sent it, which --json prints unchanged, and decodes it into view.
func callWebhook(ctx context.Context, client *tunnel.LocalClient, method, path, idempotencyKey string, body, view any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := client.Do(ctx, method, path, idempotencyKey, body, &raw); err != nil {
		return nil, err
	}
	if view != nil {
		if err := json.Unmarshal(raw, view); err != nil {
			return nil, cliError(ExitOperationFailed, "beam-agentd returned an unexpected webhook response.", "Run `beam update` so the CLI and agent match.", err)
		}
	}
	return raw, nil
}

// requireWebhookCapability refuses a command the local agent cannot serve,
// before anything is sent, with the explanation the daemon itself gives.
func requireWebhookCapability(ctx context.Context, client *tunnel.LocalClient, capability, roomID, memberID string) error {
	status, err := client.Status(ctx)
	if err != nil {
		return mapTunnelError(err)
	}
	if slices.Contains(status.Capabilities, capability) {
		return nil
	}
	return webhookFailure(roomcli.MissingWebhookCapability(capability == webhookHostCapability), roomID, memberID, nil)
}

// webhookError explains a failed webhook command, or maps it like any other
// daemon error when no explanation applies.
func webhookError(command, roomID, memberID string, err error) error {
	failure, ok := roomcli.Explain(command, err)
	if !ok {
		return mapTunnelError(err)
	}
	return webhookFailure(failure, roomID, memberID, err)
}

func webhookFailure(failure roomcli.Failure, roomID, memberID string, cause error) error {
	exit, fallback := roomExitContract(failure.Code)
	hint := failure.Hint
	if hint == "" {
		hint = fallback
	}
	if roomID != "" {
		hint = strings.ReplaceAll(hint, "ROOM_ID", roomID)
	}
	if memberID != "" {
		hint = strings.ReplaceAll(hint, "WEBHOOK_ID", memberID)
	}
	return typedCLIError(exit, failure.Kind, failure.Message, applyReleaseNames(hint), cause)
}

// confirmWebhookTrust shows what hosting a webhook means and asks for
// confirmation, unless --yes already gave it.
func (a *App) confirmWebhookTrust(renderer output.Renderer, roomID, memberID string, yes bool) error {
	renderer.Progress(fmt.Sprintf("Webhook %s in Room %s.\n%s", memberID, roomID, webhookTrustModel))
	if yes {
		return nil
	}
	if !renderer.Interactive || !a.stdinTerminal() {
		return usage("room webhook accept requires --yes to confirm the trust model when it cannot ask")
	}
	accepted, err := a.promptYesNo(renderer, "Host this webhook on this machine?", false)
	if err != nil {
		return err
	}
	if !accepted {
		return cliError(ExitInterrupted, "Webhook not accepted.", "Run the command again to accept it, or decline it with `beam room "+roomID+" webhook decline "+memberID+"`.", nil)
	}
	return nil
}

// rejectInlineSecret refuses a secret given as a command-line value, where it
// would land in shell history and process listings.
func rejectInlineSecret(args []string) error {
	for _, arg := range args {
		if arg == "--secret" || strings.HasPrefix(arg, "--secret=") {
			return usage("the webhook secret is never taken as a command-line value; use --secret-file <path> or --secret-stdin")
		}
	}
	return nil
}

// webhookSecretSource returns the --secret-file path, or reports
// --secret-stdin. Exactly one is required.
func webhookSecretSource(args []string) (string, bool, error) {
	path, fromFile, err := flag(args, "--secret-file")
	if err != nil {
		return "", false, err
	}
	fromStdin := has(args, "--secret-stdin")
	if fromFile == fromStdin {
		return "", false, usage("pass the webhook secret with exactly one of --secret-file <path> or --secret-stdin")
	}
	return path, fromStdin, nil
}

func readWebhookSecretFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", typedCLIError(ExitNotFound, "webhook_secret_invalid", "The webhook secret file could not be found.", "Check the path and try again.", err)
	}
	if !info.Mode().IsRegular() || info.Size() > webhookSecretReadLimit {
		return "", typedCLIError(ExitUsage, "webhook_secret_invalid", "The webhook secret file must be a regular file that holds only the secret.", "Write the secret alone into a private file.", nil)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", typedCLIError(ExitUsage, "webhook_secret_invalid", "The webhook secret file is accessible by other users.", "Run `chmod 600 "+path+"` and try again.", nil)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", cliError(ExitOperationFailed, "Could not read the webhook secret file.", "Check the file path and permissions.", err)
	}
	return webhookSecret(data)
}

// readWebhookSecretStdin reads the secret from standard input: typed without
// echo on a terminal, otherwise the whole input.
func (a *App) readWebhookSecretStdin(renderer output.Renderer) (string, error) {
	if a.stdinTerminal() {
		if _, err := fmt.Fprint(renderer.Err, "Webhook signing secret: "); err != nil {
			return "", err
		}
		data, err := term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(renderer.Err)
		if err != nil {
			return "", cliError(ExitInterrupted, "Could not read the webhook secret.", "Run the command again.", err)
		}
		return webhookSecret(data)
	}
	data, err := io.ReadAll(io.LimitReader(a.stdin, webhookSecretReadLimit+1))
	if err != nil {
		return "", cliError(ExitOperationFailed, "Could not read the webhook secret from standard input.", "Pipe the secret alone into the command.", err)
	}
	return webhookSecret(data)
}

// webhookSecret drops the line ending that files and echo add, and checks
// the length the host requires.
func webhookSecret(data []byte) (string, error) {
	secret := strings.TrimRight(string(data), "\r\n")
	if len(secret) < webhookSecretMinBytes || len(secret) > webhookSecretMaxBytes {
		return "", typedCLIError(ExitUsage, "webhook_secret_invalid",
			fmt.Sprintf("The webhook secret must be %d to %d bytes.", webhookSecretMinBytes, webhookSecretMaxBytes),
			"Use the signing secret the receiving system expects, for example 32 random bytes encoded as hex.", nil)
	}
	return secret, nil
}

func validateWebhookURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return usage("--url must be an absolute https URL without user information or a fragment")
	}
	return nil
}

// addWebhookDeliveryOptions copies --payload-mode and --max-file-mb into a
// request body. Left out, the coordinator's or the webhook's value applies.
func addWebhookDeliveryOptions(args []string, body map[string]any) error {
	if mode, ok, err := flag(args, "--payload-mode"); err != nil {
		return err
	} else if ok {
		if mode != "envelope" && mode != "raw" {
			return usage("--payload-mode must be envelope or raw")
		}
		body["payload_mode"] = mode
	}
	if raw, ok, err := flag(args, "--max-file-mb"); err != nil {
		return err
	} else if ok {
		value, parseErr := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if parseErr != nil || value == 0 || value > webhookMaxFileMiB {
			return usage(fmt.Sprintf("--max-file-mb must be a whole number of MiB from 1 to %d", webhookMaxFileMiB))
		}
		body["max_file_bytes"] = value << 20
	}
	return nil
}

// parseWebhookGrants reads repeated --channel CHANNEL_ID=ACTION,... values.
func parseWebhookGrants(values []string) ([]webhookGrant, error) {
	grants := make([]webhookGrant, 0, len(values))
	for _, value := range values {
		channelID, rawActions, ok := strings.Cut(value, "=")
		channelID = strings.TrimSpace(channelID)
		if !ok || channelID == "" || strings.TrimSpace(rawActions) == "" {
			return nil, usage("--channel must be CHANNEL_ID=ACTION[,ACTION...] with actions from discover, subscribe, publish")
		}
		if slices.ContainsFunc(grants, func(grant webhookGrant) bool { return grant.ChannelID == channelID }) {
			return nil, usage("--channel names channel " + channelID + " more than once; list all its actions in one value")
		}
		var actions []string
		for _, action := range strings.Split(rawActions, ",") {
			action = strings.ToLower(strings.TrimSpace(action))
			if !slices.Contains(webhookActions, action) {
				return nil, usage(fmt.Sprintf("webhook action %q is not allowed; a webhook holds only discover, subscribe, and publish", action))
			}
			if !slices.Contains(actions, action) {
				actions = append(actions, action)
			}
		}
		grants = append(grants, webhookGrant{ChannelID: channelID, Actions: actions})
	}
	return grants, nil
}

// flagValues returns every value of a repeatable option.
func flagValues(args []string, name string) []string {
	var values []string
	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == name && index+1 < len(args):
			values = append(values, args[index+1])
			index++
		case strings.HasPrefix(args[index], name+"="):
			values = append(values, strings.TrimPrefix(args[index], name+"="))
		}
	}
	return values
}

func newWebhookIdempotencyKey(action string) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("beam-webhook-%s-%d", action, time.Now().UnixNano())
	}
	return "beam-webhook-" + action + "-" + hex.EncodeToString(value[:])
}

func roomWebhooksPath(roomID string) string {
	return roomPathForCommand(roomID) + "/webhooks"
}

func hostedWebhookPath(roomID, memberID string) string {
	return "/v1/hosted-webhooks/" + url.PathEscape(roomID) + "/" + url.PathEscape(memberID)
}

// writeWebhookDetails prints one webhook and what its state means.
func writeWebhookDetails(w io.Writer, webhook webhookView) error {
	state := webhook.State
	if webhook.Reason != "" {
		state += " (" + webhook.Reason + ")"
	}
	host := "none"
	if webhook.HostMemberID != "" {
		host = webhook.HostMemberID
		if webhook.HostAgentID != "" {
			host += " (agent " + webhook.HostAgentID + ")"
		}
		if webhook.HostPresence != "" {
			host += ", " + webhook.HostPresence
		}
	}
	urlHost := "not entered yet"
	if webhook.HostName != "" {
		urlHost = webhook.HostName + " (" + webhook.URLFingerprint + ")"
	}
	rows := [][2]string{
		{"Webhook", webhook.MemberID},
		{"Room", webhook.RoomID},
		{"State", state},
		{"Host", host},
		{"URL host", urlHost},
		{"Payload", webhook.PayloadMode + ", files up to " + formatMiB(webhook.MaxFileBytes)},
	}
	// A coordinator without durable delivery sends no durable setting.
	if webhook.Durable != nil {
		durable := "off, live delivery only"
		if *webhook.Durable {
			durable = "on, its host keeps undelivered events of channels that allow receiver_local, encrypted"
		}
		rows = append(rows, [2]string{"Durable", durable})
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%-9s %s\n", row[0]+":", row[1]); err != nil {
			return err
		}
	}
	if meaning := webhookStateMeaning(webhook); meaning != "" {
		if _, err := fmt.Fprintln(w, meaning); err != nil {
			return err
		}
	}
	if blocked := webhookBlocked(webhook); blocked != "" {
		_, err := fmt.Fprintln(w, "Blocked: "+blocked+".")
		return err
	}
	return nil
}

// writeWebhooksBlocked prints, after a listing, one line for each webhook
// whose channels have members that must upgrade beam.
func writeWebhooksBlocked(w io.Writer, webhooks []webhookView) error {
	for _, webhook := range webhooks {
		if blocked := webhookBlocked(webhook); blocked != "" {
			if _, err := fmt.Fprintf(w, "Blocked webhook %s: %s.\n", webhook.MemberID, blocked); err != nil {
				return err
			}
		}
	}
	return nil
}

// webhookBlocked names the members that may control a channel the webhook
// uses and run an agent without webhook support. Until they upgrade beam the
// webhook may get no keys for those channels, whatever its state says. It is
// "" when there are none.
func webhookBlocked(webhook webhookView) string {
	switch members := webhook.UpgradeRequiredMemberIDs; len(members) {
	case 0:
		return ""
	case 1:
		return "member " + members[0] + " runs an agent without webhook support and must upgrade beam"
	default:
		return "members " + strings.Join(members, ", ") + " run agents without webhook support and must upgrade beam"
	}
}

// webhookStateMeaning explains a webhook's state and reason, and what to do
// about it, naming commands as the running binary.
func webhookStateMeaning(webhook webhookView) string {
	cli, room, member := version.CLIName(), webhook.RoomID, webhook.MemberID
	if webhookHostOffline(webhook) {
		return "Host offline: not delivering. Delivery resumes when the agent of host member " + webhook.HostMemberID +
			" is back online; nothing is kept for the webhook meanwhile."
	}
	if webhookOffline(webhook) {
		return "Webhook offline: not delivering. Its host member " + webhook.HostMemberID + " is online, and delivery starts " +
			"once the host's agent runs the webhook, seconds after it starts or accepts it; nothing is kept for the webhook " +
			"meanwhile. On the host's machine, `" + cli + " webhook hosted` shows it."
	}
	if webhookKeysPending(webhook) {
		return "Joining the encryption of " + webhookChannelCount(webhook.KeysPendingChannelIDs) + ": not delivering on " +
			webhookChannelPronoun(webhook.KeysPendingChannelIDs) + " yet (" + strings.Join(webhook.KeysPendingChannelIDs, ", ") + "). " +
			"Its host has not confirmed it holds the current keys, which after an accept, a host move or another membership " +
			"change takes a few seconds; nothing is kept for the webhook meanwhile."
	}
	switch webhook.State {
	case "pending":
		if webhook.HostMemberID == "" {
			return "Pending without a host. Name one with: " + cli + " room " + room + " webhook move " + member + " --host <member-id>"
		}
		return "Waiting for host member " + webhook.HostMemberID + " to accept it with the URL and secret, on its machine: " + cli + " room " + room + " webhook accept " + member + " --url <https-url> --secret-file <path>"
	case "active":
		return "Delivering."
	case "degraded":
		meaning := "Still delivering, but " + webhookReasonMeaning(webhook.Reason) + "."
		if webhook.Reason == "failing" {
			meaning += " Its host sends one attempt at a time until one succeeds."
		}
		return meaning
	case "paused":
		if webhook.HostMemberID == "" {
			return "Not delivering: " + webhookReasonMeaning(webhook.Reason) + ". Name a new host with: " + cli + " room " + room + " webhook move " + member + " --host <member-id>"
		}
		return "Not delivering: " + webhookReasonMeaning(webhook.Reason) + ". Resume it with: " + cli + " room " + room + " webhook resume " + member
	}
	return ""
}

// webhookHostOffline reports an active or degraded webhook whose host member
// is offline. Delivery is live only, so it delivers nothing until the host is
// back, while its state stays the same.
func webhookHostOffline(webhook webhookView) bool {
	return webhookDelivering(webhook.State) && webhook.HostPresence == "offline"
}

// webhookOffline reports an active or degraded webhook whose host is not
// offline but which is not online itself: publishes are not routed to it
// until its host's agent runs it.
func webhookOffline(webhook webhookView) bool {
	return webhookDelivering(webhook.State) && !webhookHostOffline(webhook) && webhook.Presence != "online"
}

// webhookKeysPending reports an active or degraded webhook, online with its
// host, that does not hold the current key epoch of some of its channels yet:
// it receives nothing on them until its host confirms it does.
func webhookKeysPending(webhook webhookView) bool {
	return webhookDelivering(webhook.State) && !webhookHostOffline(webhook) && !webhookOffline(webhook) &&
		len(webhook.KeysPendingChannelIDs) > 0
}

func webhookChannelCount(channelIDs []string) string {
	if len(channelIDs) == 1 {
		return "1 channel"
	}
	return fmt.Sprintf("%d channels", len(channelIDs))
}

func webhookChannelPronoun(channelIDs []string) string {
	if len(channelIDs) == 1 {
		return "it"
	}
	return "them"
}

// webhookDelivering reports a state in which a webhook delivers while it is
// online.
func webhookDelivering(state string) bool {
	return state == "active" || state == "degraded"
}

// webhookDelivery says whether a webhook delivers now.
func webhookDelivery(webhook webhookView) string {
	switch {
	case webhookHostOffline(webhook):
		return "host offline: not delivering"
	case webhookOffline(webhook):
		return "webhook offline: not delivering"
	case webhookKeysPending(webhook):
		return "joining encryption: not delivering on " + webhookChannelCount(webhook.KeysPendingChannelIDs)
	case webhookDelivering(webhook.State):
		return "delivering"
	}
	return "not delivering"
}

func webhookReasonMeaning(reason string) string {
	switch reason {
	case "failing":
		return "delivery attempts failed repeatedly"
	case "auth":
		return "the receiver rejected the signature (HTTP 401 or 403); check that both sides use the same secret, or rotate it"
	case "gone":
		return "the receiver answered HTTP 410 and asked Beam to stop"
	case "host_left":
		return "its host left the Room"
	case "host_removed":
		return "its host was removed from the Room"
	case "":
		return "no reason was reported"
	}
	return "reason " + reason
}

func webhookAttempts(result webhookTestView) string {
	text := fmt.Sprintf("%d attempt", result.Attempts)
	if result.Attempts != 1 {
		text += "s"
	}
	if result.HTTPStatus != 0 {
		text = fmt.Sprintf("HTTP %d, %s", result.HTTPStatus, text)
	}
	if result.Reason != "" {
		text += ", " + result.Reason
	}
	return text
}

func webhookTestHint(roomID, memberID, class string) string {
	switch class {
	case "auth":
		return "The receiver rejected the signature. Check that it verifies with the same secret, or rotate it with `beam room " + roomID + " webhook rotate-secret " + memberID + " --secret-file <path>`."
	case "gone":
		return "The receiver answered HTTP 410, which pauses the webhook on a real delivery. Check the URL on the receiving system."
	}
	return "Check that the receiving system is up and reachable from this machine over HTTPS. A private address needs the webhook accepted with --allow-private."
}

// hostedDelivery summarizes a hosted webhook's local delivery state.
func hostedDelivery(local *webhookLocalView) string {
	if local == nil {
		return "-"
	}
	text := "stopped"
	if local.Running {
		text = "running"
	}
	if failures := local.Health.ConsecutiveFailures; failures > 0 {
		text += fmt.Sprintf(", %d failed attempts in a row", failures)
	}
	if local.CircuitOpen {
		text += ", one attempt at a time"
	}
	if local.AsyncReplies > 0 {
		text += ", " + webhookPolledReplies(local.AsyncReplies)
	}
	spool := local.Spool
	if !local.Durable {
		if spool != nil && spool.Pending != 0 {
			text += fmt.Sprintf(", %d kept events still pending", spool.Pending)
		}
		return text
	}
	switch {
	case len(local.DurableChannelIDs) != 0:
		text += ", durable on " + webhookChannelCount(local.DurableChannelIDs)
	case local.Running:
		text += ", durable but no channel allows receiver_local"
	default:
		text += ", durable"
	}
	if spool != nil {
		text += fmt.Sprintf(": %d pending, %s, %s of %s", spool.Pending, webhookDeadLetterCount(spool.DeadLetters),
			formatSpoolBytes(spool.Bytes), formatSpoolBytes(spool.ShareBytes))
	}
	return text
}

func webhookPolledReplies(count int) string {
	if count == 1 {
		return "1 reply polled for"
	}
	return fmt.Sprintf("%d replies polled for", count)
}

// formatSpoolBytes renders a spool size in binary units.
func formatSpoolBytes(value uint64) string {
	switch {
	case value >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(value)/(1<<30))
	case value >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(value)/(1<<20))
	case value >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(value)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", value)
}

func formatMiB(value uint64) string {
	if value == 0 {
		return "-"
	}
	if value%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", value>>20)
	}
	return fmt.Sprintf("%d bytes", value)
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
