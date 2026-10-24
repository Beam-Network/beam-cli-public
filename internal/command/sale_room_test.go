//go:build !windows

package command

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/saleroom"
)

// saleRoomResponses answers the local API routes a sale Room uses, with their documented shapes.
func saleRoomResponses(method, path string) (int, any) {
	room := "btr_room_s"
	channel := "btr_channel_sale"
	switch {
	case method == http.MethodPost && path == "/v1/rooms":
		return http.StatusCreated, map[string]any{"room": map[string]any{"room_id": room}, "membership": map[string]any{"member_id": "btr_member_seller"}}
	case method == http.MethodGet && path == "/v1/rooms/"+room:
		return http.StatusOK, map[string]any{"room": map[string]any{"room_id": room}, "membership": map[string]any{"member_id": "btr_member_seller"}}
	case method == http.MethodPost && path == "/v1/rooms/"+room+"/channels":
		return http.StatusCreated, map[string]any{"channel": map[string]any{"channel_id": channel, "kind": "object"}}
	case method == http.MethodGet && path == "/v1/rooms/"+room+"/channels/"+channel:
		return http.StatusOK, map[string]any{"channel": map[string]any{"channel_id": channel, "kind": "object", "limits": map[string]any{"publication_ttl": int64(2 * time.Hour)}}}
	case method == http.MethodPost && path == "/v1/rooms/"+room+"/invitations":
		return http.StatusCreated, map[string]any{"invitation": map[string]any{"invitation_id": "btr_inv_1"}, "invitation_token": "token-value"}
	case method == http.MethodGet && path == "/v1/rooms/"+room+"/memberships":
		return http.StatusOK, map[string]any{"memberships": []any{
			map[string]any{"member_id": "btr_member_seller", "kind": "agent"},
			map[string]any{"member_id": "btr_member_buyer", "kind": "agent"},
		}}
	case method == http.MethodPost && strings.HasSuffix(path, "/objects/publish"):
		return http.StatusAccepted, map[string]any{"publication_id": "btr_pub_1", "state": "pending"}
	case method == http.MethodGet && strings.HasSuffix(path, "/objects/btr_pub_1/status"):
		return http.StatusOK, map[string]any{"status": map[string]any{"publisher": map[string]any{
			"room_transfer": map[string]any{"transfer_id": "transfer-1", "status": "completed"},
			"deliveries":    []any{map[string]any{"member_id": "btr_member_buyer", "state": "delivered"}},
		}}}
	}
	return http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "unexpected " + method + " " + path}}
}

func TestASaleRoomIsSetUpForOneBuyerAndThePackageDeliveredToItOnly(t *testing.T) {
	daemon := startWebhookDaemon(t, []string{"btr.object.publish", "btr.inbox.list", "btr.inbox.read"}, saleRoomResponses)
	app := newWebhookApp()
	cfg, paths, err := config.LoadContext("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	room, err := app.createSaleRoom(ctx, cfg, paths, "beam-key-test", 100<<30, "sale-key")
	if err != nil {
		t.Fatalf("createSaleRoom = %v", err)
	}
	if room != (saleRoom{RoomID: "btr_room_s", SellerMemberID: "btr_member_seller", ChannelID: "btr_channel_sale"}) {
		t.Fatalf("room = %+v", room)
	}
	invitation, err := app.inviteSaleBuyer(ctx, cfg, paths, room, time.Hour, "sale-key")
	if err != nil || invitation.InvitationToken != "token-value" {
		t.Fatalf("inviteSaleBuyer = %+v, %v", invitation, err)
	}
	members, err := app.saleRoomMembers(ctx, cfg, paths, room.RoomID)
	if err != nil || saleroom.CheckRoom(members, room.SellerMemberID, saleroom.Binding{MemberID: "btr_member_buyer"}) != nil {
		t.Fatalf("members = %+v, %v", members, err)
	}
	if self, err := app.saleRoomSelf(ctx, cfg, paths, room.RoomID); err != nil || self != "btr_member_seller" {
		t.Fatalf("saleRoomSelf = %q, %v", self, err)
	}
	pub, err := app.publishSalePackage(ctx, cfg, paths, room, "/data/beam-sale-x.package", "btr_member_buyer", "sale-key")
	if err != nil || pub != "btr_pub_1" {
		t.Fatalf("publishSalePackage = %q, %v", pub, err)
	}
	delivery, err := app.salePackageDelivery(ctx, cfg, paths, room, pub, "btr_member_buyer")
	if err != nil || !delivery.Done() || !delivery.Delivered() {
		t.Fatalf("salePackageDelivery = %+v, %v", delivery, err)
	}

	var sawChannel, sawInvite, sawPublish bool
	for _, call := range daemon.routeCalls() {
		switch {
		case call.Method == http.MethodPost && call.Path == "/v1/rooms":
			if call.Body["api_key"] != "beam-key-test" || call.Key != "sale-key-room" {
				t.Fatalf("room create body=%v key=%q", call.Body, call.Key)
			}
		case call.Method == http.MethodPost && strings.HasSuffix(call.Path, "/channels"):
			sawChannel = true
			limits, _ := call.Body["limits"].(map[string]any)
			wantTTL := float64(saleroom.DeliveryTTL(100<<30) / time.Second)
			if call.Body["kind"] != "object" || call.Body["visibility"] != "restricted" ||
				limits["publication_ttl_seconds"] != wantTTL || limits["max_rate_bps"] != float64(saleChannelMaxRate) {
				t.Fatalf("channel create body = %v", call.Body)
			}
		case call.Method == http.MethodPost && strings.HasSuffix(call.Path, "/invitations"):
			sawInvite = true
			access := fmt.Sprint(call.Body["channel_access"])
			if call.Body["max_uses"] != float64(1) || !strings.Contains(access, "btr_channel_sale") ||
				!strings.Contains(access, "discover") || !strings.Contains(access, "subscribe") || strings.Contains(access, "publish") {
				t.Fatalf("invitation body = %v", call.Body)
			}
		case call.Method == http.MethodPost && strings.HasSuffix(call.Path, "/objects/publish"):
			sawPublish = true
			if fmt.Sprint(call.Body["target_member_ids"]) != "[btr_member_buyer]" || call.Body["file"] != "/data/beam-sale-x.package" {
				t.Fatalf("publish body = %v", call.Body)
			}
		}
	}
	if !sawChannel || !sawInvite || !sawPublish {
		t.Fatalf("missing calls: channel %v invite %v publish %v", sawChannel, sawInvite, sawPublish)
	}
}
