//go:build !windows

package tunnel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWaitForDaemonOwnershipRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		close(release)
	}()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := WaitForDaemonOwnershipRelease(ctx, path); err != nil {
		t.Fatal(err)
	}
	<-release
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("returned while the daemon still held the lock")
	}
}

func TestWaitForDaemonOwnershipReleaseDeadlinePreservesOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := WaitForDaemonOwnershipRelease(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", err)
	}
	probe, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
		t.Fatal("timeout stole the live owner's lock")
	}
}

func TestWaitForDaemonOwnershipReleaseWithoutOwner(t *testing.T) {
	if err := WaitForDaemonOwnershipRelease(context.Background(), filepath.Join(t.TempDir(), "agent.sock")); err != nil {
		t.Fatal(err)
	}
}
