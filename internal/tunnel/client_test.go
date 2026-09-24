//go:build !windows

package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
)

func TestLocalClientContract(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Beam-CLI-Protocol") != strconv.Itoa(ProtocolVersion) {
			t.Fatalf("protocol header = %q", r.Header.Get("Beam-CLI-Protocol"))
		}
		if r.Header.Get("Beam-Local-API-Min") != strconv.Itoa(ProtocolMinVersion) || r.Header.Get("Beam-Local-API-Max") != strconv.Itoa(ProtocolVersion) {
			t.Fatalf("protocol range = %q-%q", r.Header.Get("Beam-Local-API-Min"), r.Header.Get("Beam-Local-API-Max"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == StatusPath:
			_ = json.NewEncoder(w).Encode(Status{Ready: true, AgentVersion: "1.0.0", ProtocolVersion: ProtocolVersion})
		case r.Method == http.MethodPost && r.URL.Path == ExposuresPath:
			var request ExposureRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			_ = json.NewEncoder(w).Encode(Endpoint{ID: "ep_1", Kind: request.Kind, Direction: "source", Target: request.Target, Status: "active"})
		case r.Method == http.MethodPost && r.URL.Path == DestinationsPath:
			var request DestinationRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			_ = json.NewEncoder(w).Encode(Endpoint{ID: "ep_2", Kind: "directory", Direction: "destination", Target: request.Directory, Status: "active"})
		case r.Method == http.MethodGet && r.URL.Path == EndpointsPath:
			_ = json.NewEncoder(w).Encode(EndpointsResponse{Endpoints: []Endpoint{{ID: "ep_1", Status: "active"}}})
		case r.Method == http.MethodPost && r.URL.Path == EndpointsPath+"/ep_1/close":
			_ = json.NewEncoder(w).Encode(Endpoint{ID: "ep_1", Status: "closing"})
		case r.Method == http.MethodPost && r.URL.Path == ShutdownPath:
			_ = json.NewEncoder(w).Encode(ShutdownResponse{Stopping: true})
		case r.Method == http.MethodPost && r.URL.Path == OperationsPath:
			var request OperationRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			_ = json.NewEncoder(w).Encode(Operation{ID: "op_1", Type: request.Type, Status: "queued", Request: request})
		case r.Method == http.MethodGet && r.URL.Path == OperationsPath:
			_ = json.NewEncoder(w).Encode(OperationsResponse{Operations: []Operation{{ID: "op_1", Type: "upload", Status: "running"}}})
		case r.Method == http.MethodGet && r.URL.Path == OperationsPath+"/op_1":
			_ = json.NewEncoder(w).Encode(Operation{ID: "op_1", Type: "upload", Status: "running"})
		case r.Method == http.MethodPost && r.URL.Path == OperationsPath+"/op_1/cancel":
			_ = json.NewEncoder(w).Encode(Operation{ID: "op_1", Type: "upload", Status: "cancelled"})
		case r.Method == http.MethodDelete && r.URL.Path == OperationsPath:
			_ = json.NewEncoder(w).Encode(OperationPruneResponse{RemovedIDs: []string{"op_1"}})
		case r.Method == http.MethodGet && r.URL.Path == StudioConnectionsPath:
			_ = json.NewEncoder(w).Encode(StudioConnectionStatus{Configured: true, Permissions: &StudioPermissions{MaxTunnels: 4}})
		case r.Method == http.MethodPatch && r.URL.Path == StudioConnectionsPath+"/permissions":
			var patch StudioPermissionsPatch
			_ = json.NewDecoder(r.Body).Decode(&patch)
			_ = json.NewEncoder(w).Encode(StudioConnectionStatus{Configured: true, Permissions: &StudioPermissions{MaxTunnels: *patch.MaxTunnels}})
		case r.Method == http.MethodDelete && r.URL.Path == AgentConnectionPath:
			_ = json.NewEncoder(w).Encode(AgentConnectionStatus{Prepared: true, State: "prepared"})
		case r.Method == http.MethodPost && r.URL.Path == AgentConnectionPath+"/room-invitation":
			var request AgentRoomInvitationConnectionRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if r.Header.Get("Idempotency-Key") != "join-accountless" || request.RoomID != "btr_room_test" || request.InvitationToken != "btri_secret" {
				t.Fatalf("unexpected accountless join request: %#v key=%q", request, r.Header.Get("Idempotency-Key"))
			}
			_ = json.NewEncoder(w).Encode(AgentConnectionStatus{Registered: true, AgentID: "agt_miner", OrganizationID: "org_subnet"})
		case r.Method == http.MethodGet && r.URL.Path == MetricsPath:
			_ = json.NewEncoder(w).Encode(Metrics{EndpointsTotal: 1, OperationsTotal: 1})
		default:
			http.NotFound(w, r)
		}
	})
	socket := testutil.LocalHTTPServer(t, handler)
	client := NewLocalClient(socket)
	status, err := client.Status(context.Background())
	if err != nil || !Compatible(status) {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	exposure, err := client.CreateExposure(context.Background(), ExposureRequest{Kind: "http", Target: "8080"})
	if err != nil || exposure.ID != "ep_1" {
		t.Fatalf("exposure=%#v err=%v", exposure, err)
	}
	destination, err := client.CreateDestination(context.Background(), DestinationRequest{Directory: "/tmp/incoming"})
	if err != nil || destination.Direction != "destination" {
		t.Fatalf("destination=%#v err=%v", destination, err)
	}
	endpoints, err := client.Endpoints(context.Background())
	if err != nil || len(endpoints.Endpoints) != 1 {
		t.Fatalf("endpoints=%#v err=%v", endpoints, err)
	}
	closed, err := client.Close(context.Background(), "ep_1")
	if err != nil || closed.Status != "closing" {
		t.Fatalf("closed=%#v err=%v", closed, err)
	}
	shutdown, err := client.Shutdown(context.Background())
	if err != nil || !shutdown.Stopping {
		t.Fatalf("shutdown=%#v err=%v", shutdown, err)
	}
	operation, err := client.CreateOperation(context.Background(), OperationRequest{Type: "upload", File: "/tmp/input"})
	if err != nil || operation.ID != "op_1" {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	operations, err := client.Operations(context.Background())
	if err != nil || len(operations.Operations) != 1 {
		t.Fatalf("operations=%#v err=%v", operations, err)
	}
	operation, err = client.Operation(context.Background(), "op_1")
	if err != nil || operation.Status != "running" {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	operation, err = client.CancelOperation(context.Background(), "op_1")
	if err != nil || operation.Status != "cancelled" {
		t.Fatalf("cancelled=%#v err=%v", operation, err)
	}
	pruned, err := client.PruneOperations(context.Background())
	if err != nil || len(pruned.RemovedIDs) != 1 || pruned.RemovedIDs[0] != "op_1" {
		t.Fatalf("pruned=%#v err=%v", pruned, err)
	}
	studio, err := client.StudioConnection(context.Background())
	if err != nil || studio.Permissions == nil || studio.Permissions.MaxTunnels != 4 {
		t.Fatalf("studio=%#v err=%v", studio, err)
	}
	maxTunnels := 8
	studio, err = client.UpdateStudioPermissions(context.Background(), StudioPermissionsPatch{MaxTunnels: &maxTunnels})
	if err != nil || studio.Permissions == nil || studio.Permissions.MaxTunnels != 8 {
		t.Fatalf("updated studio=%#v err=%v", studio, err)
	}
	disconnected, err := client.DisconnectAgent(context.Background())
	if err != nil || disconnected.Registered || disconnected.State != "prepared" {
		t.Fatalf("disconnected=%#v err=%v", disconnected, err)
	}
	enrolled, err := client.ConnectAgentWithRoomInvitation(context.Background(), AgentRoomInvitationConnectionRequest{
		CoordinatorURL: "https://coordinator.test", RoomID: "btr_room_test", InvitationToken: "btri_secret", LeaseTTLSeconds: 60,
	}, "join-accountless")
	if err != nil || !enrolled.Registered || enrolled.AgentID != "agt_miner" {
		t.Fatalf("accountless enrollment=%#v err=%v", enrolled, err)
	}
	metrics, err := client.Metrics(context.Background())
	if err != nil || metrics.EndpointsTotal != 1 || metrics.OperationsTotal != 1 {
		t.Fatalf("metrics=%#v err=%v", metrics, err)
	}
}

func TestLocalClientDecodesStructuredErrors(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": StructuredError{
			Code: "protocol_incompatible", Message: "no local API overlap", Retryable: false,
			Details: map[string]any{"daemon_min": 5},
		}})
	})
	_, err := NewLocalClient(testutil.LocalHTTPServer(t, handler)).Status(context.Background())
	var tunnelErr *Error
	if !errors.As(err, &tunnelErr) || tunnelErr.Kind != ErrVersion || tunnelErr.Code != "protocol_incompatible" || tunnelErr.Details["daemon_min"] != float64(5) {
		t.Fatalf("error = %#v", err)
	}
}

func TestProtocolCompatibility(t *testing.T) {
	if Compatible(Status{ProtocolVersion: ProtocolVersion + 1}) {
		t.Fatal("a different protocol version must not be compatible")
	}
}

func TestLocalClientReturnsEndpointRuntimeFailure(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != ExposuresPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Endpoint{
			ID: "ep_failed", Status: "error", Error: "publicendpoint: workload identity mismatch",
		})
	})
	socket := testutil.LocalHTTPServer(t, handler)
	endpoint, err := NewLocalClient(socket).CreateExposure(context.Background(), ExposureRequest{Kind: "file", Target: "/tmp/test"})
	if endpoint.ID != "ep_failed" {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	var tunnelErr *Error
	if !errors.As(err, &tunnelErr) || tunnelErr.Kind != ErrResponse || !strings.Contains(err.Error(), "workload identity mismatch") {
		t.Fatalf("creation error = %#v", err)
	}
}
