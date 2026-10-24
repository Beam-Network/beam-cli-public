//go:build !windows

package tunnel

import (
	"os/exec"
	"syscall"
)

func prepareDetachedCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
