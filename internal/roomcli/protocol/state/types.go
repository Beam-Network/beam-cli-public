package state

import (
	"time"

	btr "github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btr"
)

type BTRResumeHint struct {
	CoordinatorURLs      []string  `json:"coordinator_urls,omitempty"`
	ActiveCoordinatorURL string    `json:"active_coordinator_url,omitempty"`
	NotificationCursor   uint64    `json:"notification_cursor"`
	RenewAfter           time.Time `json:"renew_after,omitempty"`
	LastAttemptAt        time.Time `json:"last_attempt_at,omitempty"`
}

type BTRRoomRecord struct {
	Room               btr.Room           `json:"room"`
	Membership         btr.Membership     `json:"membership"`
	Channels           []btr.Channel      `json:"channels,omitempty"`
	Roles              []btr.Role         `json:"roles,omitempty"`
	MemberRoles        []btr.MemberRole   `json:"member_roles,omitempty"`
	Grants             []btr.ChannelGrant `json:"grants,omitempty"`
	AuthorizationEpoch uint64             `json:"authorization_epoch"`
	KeyEpoch           uint64             `json:"key_epoch"`
	Resume             BTRResumeHint      `json:"resume"`
	UpdatedAt          time.Time          `json:"updated_at"`
}
