//go:build !windows

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// WaitForDaemonOwnershipRelease waits for the agent's Unix socket owner to
// finish draining. The HTTP socket can stop accepting before that owner has
// released its room-data listener and state stores.
func WaitForDaemonOwnershipRelease(ctx context.Context, socketPath string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		lock, err := os.OpenFile(socketPath+".lock", os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect agent socket owner: %w", err)
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil
		}
		_ = lock.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("inspect agent socket owner: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent still owns its socket after shutdown: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
