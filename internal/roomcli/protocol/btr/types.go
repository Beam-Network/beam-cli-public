package btr

import "time"

const (
	MaxTokenBytes             = 256 << 10
	MaxMessageCiphertextBytes = 1 << 20
	MaxStreamRecordBytes      = 64 << 10
)

type ChannelKind string

const (
	ChannelKindMessage      ChannelKind = "message"
	ChannelKindStream       ChannelKind = "stream"
	ChannelKindDatagram     ChannelKind = "datagram"
	ChannelKindRequestReply ChannelKind = "request-reply"
	ChannelKindObject       ChannelKind = "object"
	ChannelKindMedia        ChannelKind = "media"
)

type ChannelVisibility string

type RoomState string

type MembershipState string

type PresenceState string

type InvitationState string

type RoleTemplate string

type MemberRoleState string

type ChannelState string

type ChannelGrantState string

type Action string

type GrantSubjectType string

type DeliveryReliability string

const (
	DeliveryBestEffort DeliveryReliability = "best_effort"
	DeliveryReliable   DeliveryReliability = "reliable"
)

type AcknowledgementPolicy string

const (
	AcknowledgementNone      AcknowledgementPolicy = "none"
	AcknowledgementAccepted  AcknowledgementPolicy = "accepted"
	AcknowledgementDelivered AcknowledgementPolicy = "delivered"
)

type BackpressurePolicy string

const (
	BackpressureDisconnect BackpressurePolicy = "disconnect"
	BackpressureDropOldest BackpressurePolicy = "drop_oldest"
)

type OrderingPolicy string

const (
	OrderingNone         OrderingPolicy = "none"
	OrderingPerPublisher OrderingPolicy = "per_publisher"
	OrderingPerFlow      OrderingPolicy = "per_flow"
)

type QoSClass string

const (
	QoSBulk        QoSClass = "bulk"
	QoSStandard    QoSClass = "standard"
	QoSInteractive QoSClass = "interactive"
)

type PersistenceMode string

const (
	PersistenceNone          PersistenceMode = "none"
	PersistenceSenderLocal   PersistenceMode = "sender_local"
	PersistenceReceiverLocal PersistenceMode = "receiver_local"
)

type DeliveryPolicy struct {
	Reliability     DeliveryReliability   `json:"reliability"`
	Acknowledgement AcknowledgementPolicy `json:"acknowledgement"`
	Backpressure    BackpressurePolicy    `json:"backpressure"`
}

type LocalPersistenceSelection struct {
	Mode       PersistenceMode `json:"mode"`
	BackendRef string          `json:"backend_ref,omitempty"`
}

type QoSIntent struct {
	Class            QoSClass      `json:"class"`
	MaxLatency       time.Duration `json:"max_latency"`
	PreferredRateBPS uint64        `json:"preferred_rate_bps"`
}

type PersistencePolicy struct {
	Mode         PersistenceMode   `json:"mode"`
	AllowedModes []PersistenceMode `json:"allowed_modes,omitempty"`
	MaxBytes     uint64            `json:"max_bytes,omitempty"`
	MaxAge       time.Duration     `json:"max_age,omitempty"`
}

type ResourceLimits struct {
	MaxPayloadBytes     uint64        `json:"max_payload_bytes"`
	MaxRateBPS          uint64        `json:"max_rate_bps"`
	MaxInflight         uint32        `json:"max_inflight"`
	MaxQueueMessages    uint32        `json:"max_queue_messages"`
	MaxQueueBytes       uint64        `json:"max_queue_bytes"`
	MaxSubscribers      uint32        `json:"max_subscribers"`
	MaxFanoutDegree     uint32        `json:"max_fanout_degree"`
	MaxTreeDepth        uint32        `json:"max_tree_depth"`
	DeduplicationWindow time.Duration `json:"deduplication_window"`
	PublicationTTL      time.Duration `json:"publication_ttl"`
}

type Room struct {
	RoomID             string    `json:"room_id"`
	OrganizationID     string    `json:"organization_id"`
	OwnerMemberID      string    `json:"owner_member_id"`
	OwnerPrincipalID   string    `json:"owner_principal_id"`
	State              RoomState `json:"state"`
	Version            uint64    `json:"version"`
	AuthorizationEpoch uint64    `json:"authorization_epoch"`
	PlanVersion        uint64    `json:"plan_version"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	ClosedAt           time.Time `json:"closed_at,omitempty"`
}

type Membership struct {
	MemberID           string          `json:"member_id"`
	RoomID             string          `json:"room_id"`
	PrincipalID        string          `json:"principal_id"`
	AgentID            string          `json:"agent_id"`
	Kind               string          `json:"kind"`
	ResourceID         string          `json:"resource_id,omitempty"`
	DisplayName        string          `json:"display_name,omitempty"`
	ObjectCapabilities []string        `json:"object_capabilities,omitempty"`
	State              MembershipState `json:"state"`
	Version            uint64          `json:"version"`
	Owner              bool            `json:"owner,omitempty"`
	RoleIDs            []string        `json:"role_ids,omitempty"`
	Presence           PresenceState   `json:"presence"`
	PresenceVersion    uint64          `json:"presence_version"`
	LeaseVersion       uint64          `json:"lease_version"`
	LeaseExpiresAt     time.Time       `json:"lease_expires_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	LeftAt             time.Time       `json:"left_at,omitempty"`
}

type Role struct {
	RoleID            string       `json:"role_id"`
	RoomID            string       `json:"room_id"`
	Name              string       `json:"name"`
	Template          RoleTemplate `json:"template"`
	BuiltIn           bool         `json:"built_in,omitempty"`
	Version           uint64       `json:"version"`
	CreatedByMemberID string       `json:"created_by_member_id"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

type MemberRole struct {
	RoomID             string          `json:"room_id"`
	MemberID           string          `json:"member_id"`
	RoleID             string          `json:"role_id"`
	State              MemberRoleState `json:"state"`
	Version            uint64          `json:"version"`
	AssignedByMemberID string          `json:"assigned_by_member_id"`
	AssignedAt         time.Time       `json:"assigned_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	RevokedAt          time.Time       `json:"revoked_at,omitempty"`
}

type Channel struct {
	ChannelID          string            `json:"channel_id"`
	RoomID             string            `json:"room_id"`
	Name               string            `json:"name"`
	Kind               ChannelKind       `json:"kind"`
	ContentType        string            `json:"content_type"`
	SchemaRef          string            `json:"schema_ref,omitempty"`
	Visibility         ChannelVisibility `json:"visibility"`
	State              ChannelState      `json:"state"`
	ChannelRevision    uint64            `json:"channel_revision"`
	AuthorizationEpoch uint64            `json:"authorization_epoch"`
	KeyEpoch           uint64            `json:"key_epoch"`
	RotationRequired   bool              `json:"rotation_required,omitempty"`
	Delivery           DeliveryPolicy    `json:"delivery"`
	Persistence        PersistencePolicy `json:"persistence"`
	Ordering           OrderingPolicy    `json:"ordering"`
	QoS                QoSIntent         `json:"qos"`
	Limits             ResourceLimits    `json:"limits"`
	CreatedByMemberID  string            `json:"created_by_member_id"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	ClosedAt           time.Time         `json:"closed_at,omitempty"`
}

type InvitationChannelAccess struct {
	ChannelID string   `json:"channel_id"`
	Actions   []Action `json:"actions"`
}

type Invitation struct {
	InvitationID     string                    `json:"invitation_id"`
	RoomID           string                    `json:"room_id"`
	IssuedByMemberID string                    `json:"issued_by_member_id"`
	BoundPrincipalID string                    `json:"bound_principal_id,omitempty"`
	BoundAgentID     string                    `json:"bound_agent_id,omitempty"`
	State            InvitationState           `json:"state"`
	Version          uint64                    `json:"version"`
	MaxUses          uint32                    `json:"max_uses"`
	UseCount         uint32                    `json:"use_count"`
	RoleIDs          []string                  `json:"role_ids,omitempty"`
	ChannelAccess    []InvitationChannelAccess `json:"channel_access,omitempty"`
	ExpiresAt        time.Time                 `json:"expires_at"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
	RevokedAt        time.Time                 `json:"revoked_at,omitempty"`
}

type ChannelGrant struct {
	GrantID           string            `json:"grant_id"`
	RoomID            string            `json:"room_id"`
	ChannelID         string            `json:"channel_id"`
	SubjectType       GrantSubjectType  `json:"subject_type"`
	SubjectID         string            `json:"subject_id"`
	Actions           []Action          `json:"actions"`
	State             ChannelGrantState `json:"state"`
	Version           uint64            `json:"version"`
	GrantedByMemberID string            `json:"granted_by_member_id"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	RevokedAt         time.Time         `json:"revoked_at,omitempty"`
}
