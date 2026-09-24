//go:build !windows

package tunnel

import (
	"context"
	"net"
)

func dialLocal(ctx context.Context, socketPath string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", socketPath)
}
