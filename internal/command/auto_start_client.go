package command

import (
	"context"
	"sync"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

// autoStartingTunnelClient defers daemon startup until a command performs its
// first local API request. Command handlers can therefore validate all local
// arguments before starting the companion process.
type autoStartingTunnelClient struct {
	once   sync.Once
	start  func(context.Context) (*tunnel.LocalClient, error)
	client *tunnel.LocalClient
	err    error
}

var _ tunnel.Client = (*autoStartingTunnelClient)(nil)

func (a *App) autoStartingLocalClient(cfg config.Config) tunnel.Client {
	return &autoStartingTunnelClient{
		start: func(ctx context.Context) (*tunnel.LocalClient, error) {
			return a.readyLocalClient(ctx, cfg)
		},
	}
}

func (c *autoStartingTunnelClient) ready(ctx context.Context) (*tunnel.LocalClient, error) {
	c.once.Do(func() {
		c.client, c.err = c.start(ctx)
	})
	return c.client, c.err
}

func (c *autoStartingTunnelClient) Status(ctx context.Context) (tunnel.Status, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Status{}, err
	}
	return client.Status(ctx)
}

func (c *autoStartingTunnelClient) CreateExposure(ctx context.Context, request tunnel.ExposureRequest) (tunnel.Endpoint, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Endpoint{}, err
	}
	return client.CreateExposure(ctx, request)
}

func (c *autoStartingTunnelClient) CreateDestination(ctx context.Context, request tunnel.DestinationRequest) (tunnel.Endpoint, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Endpoint{}, err
	}
	return client.CreateDestination(ctx, request)
}

func (c *autoStartingTunnelClient) Endpoints(ctx context.Context) (tunnel.EndpointsResponse, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.EndpointsResponse{}, err
	}
	return client.Endpoints(ctx)
}

func (c *autoStartingTunnelClient) Endpoint(ctx context.Context, id string) (tunnel.Endpoint, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Endpoint{}, err
	}
	return client.Endpoint(ctx, id)
}

func (c *autoStartingTunnelClient) Close(ctx context.Context, id string) (tunnel.Endpoint, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Endpoint{}, err
	}
	return client.Close(ctx, id)
}

func (c *autoStartingTunnelClient) Shutdown(ctx context.Context) (tunnel.ShutdownResponse, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.ShutdownResponse{}, err
	}
	return client.Shutdown(ctx)
}

func (c *autoStartingTunnelClient) Logs(ctx context.Context, follow bool, consume func(tunnel.LogEvent) error) error {
	client, err := c.ready(ctx)
	if err != nil {
		return err
	}
	return client.Logs(ctx, follow, consume)
}

func (c *autoStartingTunnelClient) CreateOperation(ctx context.Context, request tunnel.OperationRequest) (tunnel.Operation, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Operation{}, err
	}
	return client.CreateOperation(ctx, request)
}

func (c *autoStartingTunnelClient) Operations(ctx context.Context) (tunnel.OperationsResponse, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.OperationsResponse{}, err
	}
	return client.Operations(ctx)
}

func (c *autoStartingTunnelClient) Operation(ctx context.Context, id string) (tunnel.Operation, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Operation{}, err
	}
	return client.Operation(ctx, id)
}

func (c *autoStartingTunnelClient) CancelOperation(ctx context.Context, id string) (tunnel.Operation, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Operation{}, err
	}
	return client.CancelOperation(ctx, id)
}

func (c *autoStartingTunnelClient) PruneOperations(ctx context.Context) (tunnel.OperationPruneResponse, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.OperationPruneResponse{}, err
	}
	return client.PruneOperations(ctx)
}

func (c *autoStartingTunnelClient) Metrics(ctx context.Context) (tunnel.Metrics, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.Metrics{}, err
	}
	return client.Metrics(ctx)
}

func (c *autoStartingTunnelClient) StudioConnection(ctx context.Context) (tunnel.StudioConnectionStatus, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.StudioConnectionStatus{}, err
	}
	return client.StudioConnection(ctx)
}

func (c *autoStartingTunnelClient) ConnectStudio(ctx context.Context, request tunnel.StudioConnectionRequest) (tunnel.StudioConnectionStatus, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.StudioConnectionStatus{}, err
	}
	return client.ConnectStudio(ctx, request)
}

func (c *autoStartingTunnelClient) DisconnectStudio(ctx context.Context) (tunnel.StudioConnectionStatus, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.StudioConnectionStatus{}, err
	}
	return client.DisconnectStudio(ctx)
}

func (c *autoStartingTunnelClient) UpdateStudioPermissions(ctx context.Context, patch tunnel.StudioPermissionsPatch) (tunnel.StudioConnectionStatus, error) {
	client, err := c.ready(ctx)
	if err != nil {
		return tunnel.StudioConnectionStatus{}, err
	}
	return client.UpdateStudioPermissions(ctx, patch)
}
