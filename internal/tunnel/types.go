package tunnel

import (
	"context"
	"time"
)

const (
	ProtocolVersion       = 14
	ProtocolMinVersion    = 4
	StatusPath            = "/v1/status"
	ExposuresPath         = "/v1/exposures"
	DestinationsPath      = "/v1/destinations"
	EndpointsPath         = "/v1/endpoints"
	LogsPath              = "/v1/logs"
	ShutdownPath          = "/v1/shutdown"
	OperationsPath        = "/v1/operations"
	MetricsPath           = "/v1/metrics"
	DashboardPath         = "/v1/dashboard"
	StudioConnectionsPath = "/v1/studio-connections"
	AgentConnectionPath   = "/v1/agent-connection"
)

type Status struct {
	Ready              bool     `json:"ready"`
	DaemonVersion      string   `json:"daemon_version"`
	AgentVersion       string   `json:"agent_version,omitempty"` // legacy v3 status compatibility
	DaemonCommit       string   `json:"daemon_commit,omitempty"`
	DaemonBuildDate    string   `json:"daemon_build_date,omitempty"`
	ProtocolVersion    int      `json:"protocol_version"`
	ProtocolMinVersion int      `json:"protocol_min_version"`
	Capabilities       []string `json:"capabilities,omitempty"`
	Identity           string   `json:"identity,omitempty"`
	Message            string   `json:"message,omitempty"`
}

type ExposureRequest struct {
	Kind              string `json:"kind"`
	Target            string `json:"target"`
	Public            bool   `json:"public"`
	PublicEndpointKey string `json:"public_endpoint_key,omitempty"`
	Standbys          int    `json:"standbys,omitempty"`
	RelayID           string `json:"relay_id,omitempty"`
	ShutdownGrace     string `json:"shutdown_grace,omitempty"`
	ObjectStorage     bool   `json:"object_storage,omitempty"`
}

type DestinationRequest struct {
	Directory         string `json:"directory"`
	Public            bool   `json:"public"`
	PublicEndpointKey string `json:"public_endpoint_key,omitempty"`
	Standbys          int    `json:"standbys,omitempty"`
	RelayID           string `json:"relay_id,omitempty"`
	ShutdownGrace     string `json:"shutdown_grace,omitempty"`
	ObjectStorage     bool   `json:"object_storage,omitempty"`
}

type ObjectStorageConfig struct {
	Provider        string `json:"provider"`
	Driver          string `json:"driver"`
	EndpointURL     string `json:"endpoint_url"`
	ForcePathStyle  bool   `json:"force_path_style"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	Key             string `json:"key"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

type Endpoint struct {
	ID            string               `json:"id"`
	Kind          string               `json:"kind"`
	Direction     string               `json:"direction"`
	Target        string               `json:"target"`
	PublicURL     string               `json:"public_url,omitempty"`
	TunnelID      string               `json:"tunnel_id,omitempty"`
	Status        string               `json:"status"`
	CreatedAt     time.Time            `json:"created_at,omitempty"`
	UpdatedAt     time.Time            `json:"updated_at,omitempty"`
	Error         string               `json:"error,omitempty"`
	Public        bool                 `json:"public"`
	PublicKey     string               `json:"public_endpoint_key,omitempty"`
	ObjectStorage *ObjectStorageConfig `json:"object_storage,omitempty"`
}

type EndpointsResponse struct {
	Endpoints []Endpoint `json:"endpoints"`
}

type ShutdownResponse struct {
	Stopping bool `json:"stopping"`
}

type DashboardSession struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type StartOptions struct {
	SocketPath           string
	CoordinatorURL       string
	CoordinatorURLs      string
	DiscoverCoordinators bool
	CoordinatorRegion    string
	RelayID              string
	Transport            string
	AgentID              string
	AgentBinary          string
	CLIVersion           string
	LogPath              string
}

type OperationRequest struct {
	Type        string `json:"type"`
	File        string `json:"file,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Session     string `json:"session,omitempty"`
	WorkloadID  string `json:"workload_id,omitempty"`
	PlanVersion int64  `json:"plan_version,omitempty"`
	Manifest    string `json:"manifest,omitempty"`
	ReceiptsOut string `json:"receipts_out,omitempty"`
	UploadID    string `json:"upload_id,omitempty"`
	BridgeLease string `json:"bridge_lease,omitempty"`
	IdentityKey string `json:"identity_key,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	Listen      string `json:"listen,omitempty"`
	Target      string `json:"target,omitempty"`
	MinRelays   int    `json:"min_relays,omitempty"`
	Standbys    int    `json:"standbys,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
	PartSize    int64  `json:"part_size,omitempty"`
	Transport   string `json:"transport,omitempty"`
}

type StructuredError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

type Operation struct {
	ID          string           `json:"id"`
	Type        string           `json:"type"`
	Status      string           `json:"status"`
	Request     OperationRequest `json:"request"`
	CreatedAt   time.Time        `json:"created_at"`
	StartedAt   time.Time        `json:"started_at,omitempty"`
	CompletedAt time.Time        `json:"completed_at,omitempty"`
	UpdatedAt   time.Time        `json:"updated_at"`
	Result      map[string]any   `json:"result,omitempty"`
	Error       *StructuredError `json:"error,omitempty"`
}

type OperationsResponse struct {
	Operations []Operation `json:"operations"`
}

type OperationPruneResponse struct {
	RemovedIDs []string `json:"removed_ids"`
}

type Metrics struct {
	EndpointsTotal    int `json:"endpoints_total"`
	EndpointsActive   int `json:"endpoints_active"`
	OperationsTotal   int `json:"operations_total"`
	OperationsRunning int `json:"operations_running"`
	OperationsFailed  int `json:"operations_failed"`
}

type Diagnostics struct {
	Status  Status  `json:"status"`
	Metrics Metrics `json:"metrics"`
}

type StudioConnectionRequest struct {
	StudioURL               string   `json:"studio_url"`
	EnrollmentCode          string   `json:"enrollment_code"`
	MachineName             string   `json:"machine_name,omitempty"`
	FilesystemRoots         []string `json:"filesystem_roots,omitempty"`
	TunnelKinds             []string `json:"tunnel_kinds,omitempty"`
	NetworkTargets          []string `json:"network_targets,omitempty"`
	AllowPublic             bool     `json:"allow_public"`
	MaxTunnels              int      `json:"max_tunnels,omitempty"`
	MaxConcurrentOperations int      `json:"max_concurrent_operations,omitempty"`
	MaxCommandTTLSeconds    int      `json:"max_command_ttl_seconds,omitempty"`
	RoomsEnabled            bool     `json:"rooms_enabled"`
}

type StudioConnectionStatus struct {
	Configured     bool               `json:"configured"`
	Connected      bool               `json:"connected"`
	State          string             `json:"state"`
	StudioURL      string             `json:"studio_url,omitempty"`
	AgentID        string             `json:"agent_id,omitempty"`
	MachineID      string             `json:"machine_id,omitempty"`
	OrganizationID string             `json:"organization_id,omitempty"`
	LastError      string             `json:"last_error,omitempty"`
	ConnectedAt    time.Time          `json:"connected_at,omitempty"`
	LastSeenAt     time.Time          `json:"last_seen_at,omitempty"`
	Capabilities   []string           `json:"capabilities,omitempty"`
	Permissions    *StudioPermissions `json:"permissions,omitempty"`
}

type StudioPermissions struct {
	FilesystemRoots         []string `json:"filesystem_roots,omitempty"`
	TunnelKinds             []string `json:"tunnel_kinds,omitempty"`
	NetworkTargets          []string `json:"network_targets,omitempty"`
	AllowPublic             bool     `json:"allow_public"`
	MaxTunnels              int      `json:"max_tunnels"`
	MaxConcurrentOperations int      `json:"max_concurrent_operations"`
	MaxCommandTTLSeconds    int      `json:"max_command_ttl_seconds"`
	RoomsEnabled            bool     `json:"rooms_enabled"`
}

type StudioPermissionsPatch struct {
	FilesystemRoots         *[]string `json:"filesystem_roots,omitempty"`
	TunnelKinds             *[]string `json:"tunnel_kinds,omitempty"`
	NetworkTargets          *[]string `json:"network_targets,omitempty"`
	AllowPublic             *bool     `json:"allow_public,omitempty"`
	MaxTunnels              *int      `json:"max_tunnels,omitempty"`
	MaxConcurrentOperations *int      `json:"max_concurrent_operations,omitempty"`
	MaxCommandTTLSeconds    *int      `json:"max_command_ttl_seconds,omitempty"`
	RoomsEnabled            *bool     `json:"rooms_enabled,omitempty"`
}

type AgentConnectionPrepareRequest struct {
	CoordinatorURL string `json:"coordinator_url"`
}

type AgentConnectionRequest struct {
	CoordinatorURL  string `json:"coordinator_url"`
	EnrollmentToken string `json:"enrollment_token"`
}

type AgentRoomInvitationConnectionRequest struct {
	CoordinatorURL  string `json:"coordinator_url"`
	RoomID          string `json:"room_id"`
	InvitationToken string `json:"invitation_token"`
	LeaseTTLSeconds int    `json:"lease_ttl_seconds,omitempty"`
}

type AgentConnectionStatus struct {
	Prepared             bool      `json:"prepared"`
	Registered           bool      `json:"registered"`
	Connected            bool      `json:"connected"`
	State                string    `json:"state"`
	CoordinatorURL       string    `json:"coordinator_url,omitempty"`
	AgentID              string    `json:"agent_id,omitempty"`
	OrganizationID       string    `json:"organization_id,omitempty"`
	Label                string    `json:"label,omitempty"`
	CredentialID         string    `json:"credential_id,omitempty"`
	PublicKey            string    `json:"public_key,omitempty"`
	PublicKeyFingerprint string    `json:"public_key_fingerprint,omitempty"`
	BootID               string    `json:"boot_id,omitempty"`
	EnrolledAt           time.Time `json:"enrolled_at,omitempty"`
	LastSeenAt           time.Time `json:"last_seen_at,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
	RoomJoinOnly         bool      `json:"room_join_only,omitempty"`
	RevokedAt            time.Time `json:"revoked_at,omitempty"`
}

type StartResult struct {
	Status         Status `json:"status"`
	AlreadyRunning bool   `json:"already_running"`
	PID            int    `json:"pid,omitempty"`
	LogPath        string `json:"log_path,omitempty"`
}

type LogEvent struct {
	Time       time.Time      `json:"time,omitempty"`
	Level      string         `json:"level"`
	Message    string         `json:"message"`
	EndpointID string         `json:"endpoint_id,omitempty"`
	Fields     map[string]any `json:"fields,omitempty"`
}

type Client interface {
	Status(context.Context) (Status, error)
	CreateExposure(context.Context, ExposureRequest) (Endpoint, error)
	CreateDestination(context.Context, DestinationRequest) (Endpoint, error)
	Endpoints(context.Context) (EndpointsResponse, error)
	Endpoint(context.Context, string) (Endpoint, error)
	Close(context.Context, string) (Endpoint, error)
	Shutdown(context.Context) (ShutdownResponse, error)
	Logs(context.Context, bool, func(LogEvent) error) error
	CreateOperation(context.Context, OperationRequest) (Operation, error)
	Operations(context.Context) (OperationsResponse, error)
	Operation(context.Context, string) (Operation, error)
	CancelOperation(context.Context, string) (Operation, error)
	PruneOperations(context.Context) (OperationPruneResponse, error)
	Metrics(context.Context) (Metrics, error)
	StudioConnection(context.Context) (StudioConnectionStatus, error)
	ConnectStudio(context.Context, StudioConnectionRequest) (StudioConnectionStatus, error)
	DisconnectStudio(context.Context) (StudioConnectionStatus, error)
	UpdateStudioPermissions(context.Context, StudioPermissionsPatch) (StudioConnectionStatus, error)
}
