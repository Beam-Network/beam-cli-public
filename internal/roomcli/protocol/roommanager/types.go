package roommanager

import btr "github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btr"

type PersistenceInput struct {
	Mode          btr.PersistenceMode   `json:"mode"`
	AllowedModes  []btr.PersistenceMode `json:"allowed_modes,omitempty"`
	MaxBytes      uint64                `json:"max_bytes,omitempty"`
	MaxAgeSeconds int64                 `json:"max_age_seconds,omitempty"`
}

type QoSInput struct {
	Class            btr.QoSClass `json:"class"`
	MaxLatencyMillis int64        `json:"max_latency_millis"`
	PreferredRateBPS uint64       `json:"preferred_rate_bps"`
}

type LimitsInput struct {
	MaxPayloadBytes            uint64 `json:"max_payload_bytes"`
	MaxRateBPS                 uint64 `json:"max_rate_bps"`
	MaxInflight                uint32 `json:"max_inflight"`
	MaxQueueMessages           uint32 `json:"max_queue_messages"`
	MaxQueueBytes              uint64 `json:"max_queue_bytes"`
	MaxSubscribers             uint32 `json:"max_subscribers"`
	MaxFanoutDegree            uint32 `json:"max_fanout_degree"`
	MaxTreeDepth               uint32 `json:"max_tree_depth"`
	DeduplicationWindowSeconds int64  `json:"deduplication_window_seconds"`
	PublicationTTLSeconds      int64  `json:"publication_ttl_seconds"`
}

type ChannelPolicyInput struct {
	Name        string                `json:"name"`
	Kind        btr.ChannelKind       `json:"kind,omitempty"`
	ContentType string                `json:"content_type"`
	SchemaRef   string                `json:"schema_ref,omitempty"`
	Visibility  btr.ChannelVisibility `json:"visibility"`
	Delivery    btr.DeliveryPolicy    `json:"delivery"`
	Persistence PersistenceInput      `json:"persistence"`
	Ordering    btr.OrderingPolicy    `json:"ordering"`
	QoS         QoSInput              `json:"qos"`
	Limits      LimitsInput           `json:"limits"`
}

type CreateChannelInput struct {
	ChannelPolicyInput
	ExpectedAuthorizationEpoch uint64 `json:"expected_authorization_epoch"`
}

type UpdateChannelInput struct {
	ChannelPolicyInput
	ExpectedAuthorizationEpoch uint64 `json:"expected_authorization_epoch"`
	ExpectedChannelRevision    uint64 `json:"expected_channel_revision"`
}

type PutGrantInput struct {
	SubjectType                btr.GrantSubjectType `json:"subject_type"`
	SubjectID                  string               `json:"subject_id"`
	Actions                    []btr.Action         `json:"actions"`
	ExpectedAuthorizationEpoch uint64               `json:"expected_authorization_epoch"`
	ExpectedChannelRevision    uint64               `json:"expected_channel_revision"`
}

type InvitationResult struct {
	Invitation      btr.Invitation `json:"invitation"`
	InvitationToken string         `json:"invitation_token"`
}
