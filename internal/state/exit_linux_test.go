//go:build linux

package state

import (
	"os/exec"
	"testing"
)

func TestProcessExitCodeMapsSignalToShellConvention(t *testing.T) {
	command := exec.Command("sh", "-c", "kill -TERM $$")
	err := command.Run()
	processExit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected signal exit error, got %T (%v)", err, err)
	}
	code, ok := processExitCode(processExit)
	if !ok || code != 143 {
		t.Fatalf("processExitCode() = %d, %v; want 143, true", code, ok)
	}
}
