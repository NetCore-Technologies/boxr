//go:build linux

package state

import (
	"os/exec"
	"syscall"
)

func processExitCode(processExit *exec.ExitError) (int, bool) {
	if processExit == nil {
		return 0, false
	}
	status, ok := processExit.Sys().(syscall.WaitStatus)
	if !ok {
		return 0, false
	}
	if status.Exited() {
		return status.ExitStatus(), true
	}
	if status.Signaled() {
		return 128 + int(status.Signal()), true
	}
	return 0, false
}
