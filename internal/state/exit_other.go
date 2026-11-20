//go:build !linux

package state

import "os/exec"

func processExitCode(processExit *exec.ExitError) (int, bool) {
	if processExit == nil || processExit.ExitCode() < 0 {
		return 0, false
	}
	return processExit.ExitCode(), true
}
