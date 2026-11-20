//go:build linux

package container

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestRunRejectsEmptyCommand(t *testing.T) {
	if err := Run("", nil, Options{}); err == nil || !strings.Contains(err.Error(), "command cannot be empty") {
		t.Fatalf("Run(empty) error = %v, want empty-command error", err)
	}
}

func TestRunReportsMissingExecutableBeforeNamespaceSetup(t *testing.T) {
	err := Run("boxr-test-command-that-does-not-exist-7f19d1", nil, Options{})
	if err == nil || !strings.Contains(err.Error(), "find command") {
		t.Fatalf("Run(missing executable) error = %v, want lookup error", err)
	}
}

func TestInitRejectsMalformedInternalArguments(t *testing.T) {
	if err := Init(nil); err == nil || !strings.Contains(err.Error(), "invalid internal init arguments") {
		t.Fatalf("Init(nil) error = %v, want invalid-arguments error", err)
	}
}

func TestRunAsInitPropagatesSuccessfulExit(t *testing.T) {
	if err := runAsInit([]string{"/bin/sh", "-c", "exit 0"}, make(chan os.Signal)); err != nil {
		t.Fatalf("runAsInit(success) = %v, want nil", err)
	}
}

func TestRunAsInitPropagatesNonZeroExit(t *testing.T) {
	err := runAsInit([]string{"/bin/sh", "-c", "exit 23"}, make(chan os.Signal))
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runAsInit(nonzero) error = %v, want *ExitError", err)
	}
	if exitErr.Code != 23 {
		t.Fatalf("exit code = %d, want 23", exitErr.Code)
	}
}

func TestRunAsInitConvertsSignalExitCode(t *testing.T) {
	err := runAsInit([]string{"/bin/sh", "-c", "kill -TERM $$"}, make(chan os.Signal))
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runAsInit(signal) error = %v, want *ExitError", err)
	}
	if exitErr.Code != 128+int(syscall.SIGTERM) {
		t.Fatalf("signal exit code = %d, want %d", exitErr.Code, 128+int(syscall.SIGTERM))
	}
}

func TestExitErrorHasUsefulMessage(t *testing.T) {
	if got := (&ExitError{Code: 9}).Error(); got != "container command exited with status 9" {
		t.Fatalf("ExitError.Error() = %q", got)
	}
}
