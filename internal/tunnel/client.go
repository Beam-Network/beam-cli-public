package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ErrorKind string

const (
	ErrUnavailable ErrorKind = "unavailable"
	ErrVersion     ErrorKind = "version"
	ErrNotFound    ErrorKind = "not_found"
	ErrConflict    ErrorKind = "conflict"
	ErrResponse    ErrorKind = "response"
)

type Error struct {
	Kind      ErrorKind
	Status    int
	Code      string
	Detail    string
	Retryable bool
	Details   map[string]any
	Cause     error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "beam-agent request failed"
}

func (e *Error) Unwrap() error { return e.Cause }

type LocalClient struct {
	SocketPath      string
	CLIVersion      string
	ProtocolMinimum int
	ProtocolMaximum int
	HTTPClient      *http.Client
}

func (c *LocalClient) WithCLIVersion(version string) *LocalClient {
	c.CLIVersion = version
	return c
}

// WithProtocolRange narrows local API negotiation for a command family that
// requires a newer daemon capability than the core tunnel client.
func (c *LocalClient) WithProtocolRange(minimum, maximum int) *LocalClient {
	c.ProtocolMinimum = minimum
	c.ProtocolMaximum = maximum
	return c
}

func NewLocalClient(socketPath string) *LocalClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialLocal(ctx, socketPath)
		},
		DisableCompression: true,
		// Public endpoint creation can include DNS publication and the first ACME
		// issuance. Keep the local API request alive for that bounded operation.
		ResponseHeaderTimeout: 150 * time.Second,
	}
	return &LocalClient{
		SocketPath: socketPath,
		HTTPClient: &http.Client{Transport: transport},
	}
}

func (c *LocalClient) Status(ctx context.Context) (Status, error) {
	var result Status
	err := c.do(ctx, http.MethodGet, StatusPath, nil, &result)
	return result, err
}

func (c *LocalClient) CreateExposure(ctx context.Context, request ExposureRequest) (Endpoint, error) {
	var result Endpoint
	err := c.do(ctx, http.MethodPost, ExposuresPath, request, &result)
	if err == nil {
		err = endpointCreationError(result)
	}
	return result, err
}

func (c *LocalClient) CreateDestination(ctx context.Context, request DestinationRequest) (Endpoint, error) {
	var result Endpoint
	err := c.do(ctx, http.MethodPost, DestinationsPath, request, &result)
	if err == nil {
		err = endpointCreationError(result)
	}
	return result, err
}

func endpointCreationError(endpoint Endpoint) error {
	if !strings.EqualFold(strings.TrimSpace(endpoint.Status), "error") {
		return nil
	}
	detail := strings.TrimSpace(endpoint.Error)
	if detail == "" {
		detail = fmt.Sprintf("Tunnel endpoint %s failed during creation.", endpoint.ID)
	}
	return &Error{Kind: ErrResponse, Detail: detail}
}

func (c *LocalClient) Endpoints(ctx context.Context) (EndpointsResponse, error) {
	var result EndpointsResponse
	err := c.do(ctx, http.MethodGet, EndpointsPath, nil, &result)
	return result, err
}

func (c *LocalClient) Endpoint(ctx context.Context, id string) (Endpoint, error) {
	var result Endpoint
	err := c.do(ctx, http.MethodGet, EndpointsPath+"/"+url.PathEscape(id), nil, &result)
	return result, err
}

func (c *LocalClient) Close(ctx context.Context, id string) (Endpoint, error) {
	var result Endpoint
	err := c.do(ctx, http.MethodPost, EndpointsPath+"/"+url.PathEscape(id)+"/close", struct{}{}, &result)
	return result, err
}

func (c *LocalClient) Shutdown(ctx context.Context) (ShutdownResponse, error) {
	var result ShutdownResponse
	err := c.do(ctx, http.MethodPost, ShutdownPath, struct{}{}, &result)
	return result, err
}

func (c *LocalClient) StartDashboard(ctx context.Context) (DashboardSession, error) {
	var result DashboardSession
	err := c.do(ctx, http.MethodPost, DashboardPath, struct{}{}, &result)
	return result, err
}

func (c *LocalClient) Logs(ctx context.Context, follow bool, consume func(LogEvent) error) error {
	path := LogsPath
	if follow {
		path += "?follow=true"
	}
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return err
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 64<<20))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event LogEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return &Error{Kind: ErrResponse, Detail: "beam-agent returned an invalid log event.", Cause: err}
		}
		if err := consume(event); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return &Error{Kind: ErrResponse, Detail: "Could not follow beam-agent logs.", Cause: err}
	}
	return nil
}

func (c *LocalClient) CreateOperation(ctx context.Context, request OperationRequest) (Operation, error) {
	var result Operation
	err := c.do(ctx, http.MethodPost, OperationsPath, request, &result)
	return result, err
}

func (c *LocalClient) Operations(ctx context.Context) (OperationsResponse, error) {
	var result OperationsResponse
	err := c.do(ctx, http.MethodGet, OperationsPath, nil, &result)
	return result, err
}

func (c *LocalClient) Operation(ctx context.Context, id string) (Operation, error) {
	var result Operation
	err := c.do(ctx, http.MethodGet, OperationsPath+"/"+url.PathEscape(id), nil, &result)
	return result, err
}

func (c *LocalClient) CancelOperation(ctx context.Context, id string) (Operation, error) {
	var result Operation
	err := c.do(ctx, http.MethodPost, OperationsPath+"/"+url.PathEscape(id)+"/cancel", struct{}{}, &result)
	return result, err
}

func (c *LocalClient) PruneOperations(ctx context.Context) (OperationPruneResponse, error) {
	var result OperationPruneResponse
	err := c.do(ctx, http.MethodDelete, OperationsPath, nil, &result)
	return result, err
}

func (c *LocalClient) Metrics(ctx context.Context) (Metrics, error) {
	var result Metrics
	err := c.do(ctx, http.MethodGet, MetricsPath, nil, &result)
	return result, err
}

func (c *LocalClient) StudioConnection(ctx context.Context) (StudioConnectionStatus, error) {
	var result StudioConnectionStatus
	err := c.do(ctx, http.MethodGet, StudioConnectionsPath, nil, &result)
	return result, err
}

func (c *LocalClient) ConnectStudio(ctx context.Context, request StudioConnectionRequest) (StudioConnectionStatus, error) {
	var result StudioConnectionStatus
	err := c.do(ctx, http.MethodPost, StudioConnectionsPath, request, &result)
	return result, err
}

func (c *LocalClient) DisconnectStudio(ctx context.Context) (StudioConnectionStatus, error) {
	var result StudioConnectionStatus
	err := c.do(ctx, http.MethodDelete, StudioConnectionsPath, nil, &result)
	return result, err
}

func (c *LocalClient) UpdateStudioPermissions(ctx context.Context, patch StudioPermissionsPatch) (StudioConnectionStatus, error) {
	var result StudioConnectionStatus
	err := c.do(ctx, http.MethodPatch, StudioConnectionsPath+"/permissions", patch, &result)
	return result, err
}

func (c *LocalClient) AgentConnection(ctx context.Context) (AgentConnectionStatus, error) {
	var result AgentConnectionStatus
	err := c.do(ctx, http.MethodGet, AgentConnectionPath, nil, &result)
	return result, err
}

func (c *LocalClient) PrepareAgentConnection(ctx context.Context, coordinatorURL string) (AgentConnectionStatus, error) {
	var result AgentConnectionStatus
	err := c.do(ctx, http.MethodPost, AgentConnectionPath+"/prepare", AgentConnectionPrepareRequest{CoordinatorURL: coordinatorURL}, &result)
	return result, err
}

func (c *LocalClient) ConnectAgent(ctx context.Context, request AgentConnectionRequest) (AgentConnectionStatus, error) {
	var result AgentConnectionStatus
	err := c.do(ctx, http.MethodPost, AgentConnectionPath, request, &result)
	return result, err
}

func (c *LocalClient) ConnectAgentWithRoomInvitation(ctx context.Context, request AgentRoomInvitationConnectionRequest, idempotencyKey string) (AgentConnectionStatus, error) {
	var result AgentConnectionStatus
	err := c.Do(ctx, http.MethodPost, AgentConnectionPath+"/room-invitation", idempotencyKey, request, &result)
	return result, err
}

func (c *LocalClient) RecoverAgent(ctx context.Context) (AgentConnectionStatus, error) {
	var result AgentConnectionStatus
	err := c.do(ctx, http.MethodPost, AgentConnectionPath+"/recover", struct{}{}, &result)
	return result, err
}

func (c *LocalClient) MarkAgentRevoked(ctx context.Context) (AgentConnectionStatus, error) {
	var result AgentConnectionStatus
	err := c.do(ctx, http.MethodPost, AgentConnectionPath+"/revoked", struct{}{}, &result)
	return result, err
}

func (c *LocalClient) DisconnectAgent(ctx context.Context) (AgentConnectionStatus, error) {
	var result AgentConnectionStatus
	err := c.do(ctx, http.MethodDelete, AgentConnectionPath, nil, &result)
	return result, err
}

// Do performs a version-negotiated JSON request against beam-agentd's local
// API. It is exported for command families, such as Rooms, whose API surface
// evolves independently from the core tunnel endpoints.
func (c *LocalClient) Do(ctx context.Context, method, path, idempotencyKey string, body, out any) error {
	resp, err := c.requestJSON(ctx, method, path, idempotencyKey, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return &Error{Kind: ErrResponse, Status: resp.StatusCode, Detail: "beam-agent returned invalid JSON.", Cause: err}
	}
	return nil
}

// DoLong has the same cancellation semantics as Do. The local transport has no
// global request timeout, so long-running Room submissions remain governed by
// the caller context and the daemon's own bounds.
func (c *LocalClient) DoLong(ctx context.Context, method, path, idempotencyKey string, body, out any) error {
	return c.Do(ctx, method, path, idempotencyKey, body, out)
}

// DoRaw sends an application stream to a Room adapter endpoint.
func (c *LocalClient) DoRaw(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return &Error{Kind: ErrResponse, Status: resp.StatusCode, Detail: "beam-agent returned invalid JSON.", Cause: err}
	}
	return nil
}

// OpenStream opens a successful local response without consuming its body.
// The caller owns the returned response body.
func (c *LocalClient) OpenStream(ctx context.Context, path string) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if err := checkResponse(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (c *LocalClient) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return &Error{Kind: ErrResponse, Status: resp.StatusCode, Detail: "beam-agent returned invalid JSON.", Cause: err}
	}
	return nil
}

func (c *LocalClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return c.requestJSON(ctx, method, path, "", body)
}

func (c *LocalClient) requestJSON(ctx context.Context, method, path, idempotencyKey string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := c.newRequest(ctx, method, path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return c.send(req)
}

func (c *LocalClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://beam-agent"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	minimum, maximum := c.ProtocolMinimum, c.ProtocolMaximum
	if minimum == 0 {
		minimum = ProtocolMinVersion
	}
	if maximum == 0 {
		maximum = ProtocolVersion
	}
	req.Header.Set("Beam-CLI-Protocol", fmt.Sprint(maximum))
	req.Header.Set("Beam-Local-API-Min", fmt.Sprint(minimum))
	req.Header.Set("Beam-Local-API-Max", fmt.Sprint(maximum))
	if c.CLIVersion != "" {
		req.Header.Set("Beam-CLI-Version", c.CLIVersion)
	}
	return req, nil
}

func (c *LocalClient) send(req *http.Request) (*http.Response, error) {
	client := c.HTTPClient
	if client == nil {
		client = NewLocalClient(c.SocketPath).HTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) || strings.Contains(err.Error(), c.SocketPath) {
			return nil, &Error{Kind: ErrUnavailable, Detail: "beam-agent is unavailable.", Cause: err}
		}
		return nil, &Error{Kind: ErrUnavailable, Detail: "beam-agent is unavailable.", Cause: err}
	}
	return resp, nil
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(data, &payload)
	detail := payload.Message
	var structured StructuredError
	if len(payload.Error) != 0 {
		if payload.Error[0] == '"' {
			var legacyCode string
			_ = json.Unmarshal(payload.Error, &legacyCode)
			structured.Code = legacyCode
			if detail == "" {
				detail = legacyCode
			}
		} else {
			_ = json.Unmarshal(payload.Error, &structured)
			if detail == "" {
				detail = structured.Message
			}
		}
	}
	if detail == "" {
		detail = fmt.Sprintf("beam-agent request failed (HTTP %d).", resp.StatusCode)
	}
	kind := ErrResponse
	switch resp.StatusCode {
	case http.StatusNotFound:
		kind = ErrNotFound
	case http.StatusConflict:
		kind = ErrConflict
	case http.StatusUpgradeRequired:
		kind = ErrVersion
	}
	return &Error{Kind: kind, Status: resp.StatusCode, Code: structured.Code, Detail: detail, Retryable: structured.Retryable, Details: structured.Details}
}

func Compatible(status Status) bool {
	minimum := status.ProtocolMinVersion
	if minimum == 0 {
		minimum = status.ProtocolVersion
	}
	return status.ProtocolVersion >= ProtocolMinVersion && minimum <= ProtocolVersion
}

func VersionError(status Status) error {
	return fmt.Errorf("beam-agentd local API %d-%d is incompatible with CLI local API %d-%d", status.ProtocolMinVersion, status.ProtocolVersion, ProtocolMinVersion, ProtocolVersion)
}

var _ Client = (*LocalClient)(nil)
