//go:build windows

package tunnel

import "context"

// Windows agent shutdown is synchronized by its local transport lifecycle.
func WaitForDaemonOwnershipRelease(context.Context, string) error { return nil }
