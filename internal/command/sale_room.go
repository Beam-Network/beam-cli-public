package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btr"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/roommanager"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/state"
	"github.com/Beam-Network/beam-cli-public/internal/saleroom"
)

// saleChannelName names the object channel a sale Room carries its package on.
const saleChannelName = "sale"

// saleChannelMaxRate is the sale channel's declared rate limit. A dataset of up to 100 GB must cross it within the
// delivery window, so it is set well above the 1 MiB/s default for new channels.
const saleChannelMaxRate = 1 << 30

// saleRoomJSON runs one Room command through `beam room`'s own path, so it starts the agent, checks Room control and
// enrolls a first-time joiner exactly as `beam room` would, and returns its JSON output.
func (a *App) saleRoomJSON(ctx context.Context, cfg config.Config, paths config.Paths, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	renderer := output.Renderer{Mode: output.JSON, Out: &stdout, Err: &stderr}
	if err := a.roomCommand(ctx, args, cfg, paths, renderer); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func decodeSaleRoom[T any](data []byte, what string) (T, error) {
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return value, cliError(ExitOperationFailed, "The agent returned an unreadable "+what+".", "Retry the command.", err)
	}
	return value, nil
}

// saleRoom is a Room set up for one sale: its id, the seller's member and the object channel.
type saleRoom struct {
	RoomID         string `json:"room_id"`
	SellerMemberID string `json:"seller_member_id"`
	ChannelID      string `json:"channel_id"`
}

// createSaleRoom starts a Room (billable, charged to apiKey or $BEAM_API_KEY) and its object channel, whose
// publication window lets a delivery of sizeBytes complete.
func (a *App) createSaleRoom(ctx context.Context, cfg config.Config, paths config.Paths, apiKey string, sizeBytes uint64, key string) (saleRoom, error) {
	args := []string{"create", "--idempotency-key", key + "-room"}
	if apiKey != "" {
		args = append(args, "--api-key", apiKey)
	}
	data, err := a.saleRoomJSON(ctx, cfg, paths, args...)
	if err != nil {
		return saleRoom{}, err
	}
	record, err := decodeSaleRoom[state.BTRRoomRecord](data, "Room")
	if err != nil {
		return saleRoom{}, err
	}
	room := saleRoom{RoomID: record.Room.RoomID, SellerMemberID: record.Membership.MemberID}
	data, err = a.saleRoomJSON(ctx, cfg, paths, "channel", "create", room.RoomID,
		"--name", saleChannelName, "--kind", "object", "--visibility", "restricted",
		"--publication-ttl", saleroom.DeliveryTTL(sizeBytes).String(),
		"--max-rate", strconv.Itoa(saleChannelMaxRate), "--idempotency-key", key+"-channel")
	if err != nil {
		return saleRoom{}, err
	}
	channel, err := decodeSaleRoom[struct {
		Channel btr.Channel `json:"channel"`
	}](data, "channel")
	if err != nil {
		return saleRoom{}, err
	}
	room.ChannelID = channel.Channel.ChannelID
	return room, nil
}

// inviteSaleBuyer creates the one-use invitation the buyer joins with. It grants only discovering and receiving on
// the sale channel.
func (a *App) inviteSaleBuyer(ctx context.Context, cfg config.Config, paths config.Paths, room saleRoom, ttl time.Duration, key string) (roommanager.InvitationResult, error) {
	data, err := a.saleRoomJSON(ctx, cfg, paths, "invite", room.RoomID, "--max-uses", "1",
		"--ttl", strconv.Itoa(int(ttl/time.Second)),
		"--channel-access", room.ChannelID+"=discover,subscribe", "--idempotency-key", key+"-invite")
	if err != nil {
		return roommanager.InvitationResult{}, err
	}
	return decodeSaleRoom[roommanager.InvitationResult](data, "invitation")
}

// saleRoomMembers lists every membership of the Room, for the two-member rule.
func (a *App) saleRoomMembers(ctx context.Context, cfg config.Config, paths config.Paths, roomID string) ([]saleroom.Member, error) {
	data, err := a.saleRoomJSON(ctx, cfg, paths, "members", roomID)
	if err != nil {
		return nil, err
	}
	list, err := decodeSaleRoom[struct {
		Memberships []btr.Membership `json:"memberships"`
	}](data, "member list")
	if err != nil {
		return nil, err
	}
	members := make([]saleroom.Member, 0, len(list.Memberships))
	for _, m := range list.Memberships {
		members = append(members, saleroom.Member{MemberID: m.MemberID, Kind: string(m.Kind)})
	}
	return members, nil
}

// saleRoomSelf returns the local agent's member id in the Room.
func (a *App) saleRoomSelf(ctx context.Context, cfg config.Config, paths config.Paths, roomID string) (string, error) {
	data, err := a.saleRoomJSON(ctx, cfg, paths, "inspect", roomID)
	if err != nil {
		return "", err
	}
	record, err := decodeSaleRoom[state.BTRRoomRecord](data, "Room")
	if err != nil {
		return "", err
	}
	return record.Membership.MemberID, nil
}

// publishSalePackage sends the package file to the buyer's member only and returns the publication id.
func (a *App) publishSalePackage(ctx context.Context, cfg config.Config, paths config.Paths, room saleRoom, path, buyerMemberID, key string) (string, error) {
	data, err := a.saleRoomJSON(ctx, cfg, paths, "channel", "object", "publish", room.RoomID, room.ChannelID,
		"--file", path, "--to", buyerMemberID, "--idempotency-key", key+"-publish")
	if err != nil {
		return "", err
	}
	var response any
	if err := json.Unmarshal(data, &response); err != nil {
		return "", cliError(ExitOperationFailed, "The agent returned an unreadable publication.", "Check the publication with `beam room channel object list`.", err)
	}
	id := findString(response, "publication_id")
	if id == "" {
		return "", cliError(ExitOperationFailed, "The agent did not return a publication id.", "Check the publication with `beam room channel object list`.", nil)
	}
	return id, nil
}

// saleDelivery is the state of the package's delivery to the buyer.
type saleDelivery struct {
	TransferState string // the Room transfer's status
	MemberState   string // the buyer's delivery state
	Reason        string // why the buyer's delivery is unavailable, when it is
}

// Done reports whether the delivery has ended, and Delivered whether it reached the buyer.
func (d saleDelivery) Done() bool {
	switch d.TransferState {
	case "completed", "partial", "failed", "cancelled", "expired", "rejected":
		return true
	}
	return false
}

func (d saleDelivery) Delivered() bool {
	return d.TransferState == "completed" || d.MemberState == "delivered"
}

// salePackageDelivery reads the publication's status for the buyer's member.
func (a *App) salePackageDelivery(ctx context.Context, cfg config.Config, paths config.Paths, room saleRoom, publicationID, buyerMemberID string) (saleDelivery, error) {
	data, err := a.saleRoomJSON(ctx, cfg, paths, "channel", "object", "status", room.RoomID, room.ChannelID, publicationID)
	if err != nil {
		return saleDelivery{}, err
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		return saleDelivery{}, cliError(ExitOperationFailed, "The agent returned an unreadable publication status.", "Retry the command.", err)
	}
	view, _ := response["status"].(map[string]any)
	for _, side := range []string{"publisher", "recipient"} {
		if inner, ok := view[side].(map[string]any); ok {
			view = inner
			break
		}
	}
	delivery := saleDelivery{}
	if transfer, ok := view["room_transfer"].(map[string]any); ok {
		delivery.TransferState, _ = transfer["status"].(string)
	}
	if delivery.TransferState == "" {
		delivery.TransferState, _ = view["state"].(string)
	}
	if deliveries, ok := view["deliveries"].([]any); ok {
		for _, raw := range deliveries {
			d, _ := raw.(map[string]any)
			if d["member_id"] == buyerMemberID {
				delivery.MemberState, _ = d["state"].(string)
				delivery.Reason, _ = d["unavailable_reason"].(string)
			}
		}
	}
	return delivery, nil
}

// findString returns the first string value under key anywhere in a decoded JSON document.
func findString(value any, key string) string {
	switch v := value.(type) {
	case map[string]any:
		if s, ok := v[key].(string); ok && s != "" {
			return s
		}
		for _, inner := range v {
			if s := findString(inner, key); s != "" {
				return s
			}
		}
	case []any:
		for _, inner := range v {
			if s := findString(inner, key); s != "" {
				return s
			}
		}
	}
	return ""
}

// describeDeliveryFailure turns an ended, undelivered publication into an error the seller can act on.
func describeDeliveryFailure(d saleDelivery) error {
	reason := d.Reason
	if reason == "" {
		reason = d.TransferState
	}
	return typedCLIError(ExitOperationFailed, "sale_delivery_"+d.TransferState,
		fmt.Sprintf("The sale package did not reach the buyer (%s).", reason),
		"Check that the buyer's agent is online and has joined the Room, then offer again.", nil)
}
