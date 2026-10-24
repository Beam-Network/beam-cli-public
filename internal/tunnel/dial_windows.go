//go:build windows

package tunnel

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

func dialLocal(ctx context.Context, socketPath string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, socketPath)
}
