//go:build windows

package tunnel

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrepareDetachedCommandUsesWindowsProcessGroup(t *testing.T) {
	command := exec.Command("cmd", "/C", "exit", "0")
	prepareDetachedCommand(command)
	if command.SysProcAttr == nil {
		t.Fatal("missing Windows process attributes")
	}
	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	if command.SysProcAttr.CreationFlags&want != want {
		t.Fatalf("creation flags = %#x, want %#x", command.SysProcAttr.CreationFlags, want)
	}
}
