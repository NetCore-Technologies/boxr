//go:build linux

package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestChildProcessEnvironmentDropsHostVariablesForBuilds(t *testing.T) {
	values := childProcessEnvironment(Options{
		ClearEnv: true,
		Env:      []string{"BUILD_MODE=release", "PATH=/build/bin"},
	}, []string{"HOST_SECRET=should-not-leak", "PATH=/host/bin", "HOME=/home/test"})
	if got, ok := testEnvironmentValue(values, "HOST_SECRET"); ok {
		t.Fatalf("host secret leaked into build environment: %q", got)
	}
	if got, ok := testEnvironmentValue(values, "PATH"); !ok || got != "/build/bin" {
		t.Fatalf("PATH = %q, present=%v", got, ok)
	}
	if got, ok := testEnvironmentValue(values, "HOME"); !ok || got != "/root" {
		t.Fatalf("HOME = %q, present=%v", got, ok)
	}
	if got, ok := testEnvironmentValue(values, "BUILD_MODE"); !ok || got != "release" {
		t.Fatalf("BUILD_MODE = %q, present=%v", got, ok)
	}
}

func testEnvironmentValue(values []string, key string) (string, bool) {
	for _, value := range values {
		name, content, ok := strings.Cut(value, "=")
		if ok && name == key {
			return content, true
		}
	}
	return "", false
}


func TestRunAsInitForwardsUserSignalsToWorkloadGroup(t *testing.T) {
	cases := []struct {
		name     string
		signal   syscall.Signal
		exitCode int
		trapName string
	}{
		{name: "USR1", signal: syscall.SIGUSR1, exitCode: 41, trapName: "USR1"},
		{name: "USR2", signal: syscall.SIGUSR2, exitCode: 42, trapName: "USR2"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			readyPath := filepath.Join(t.TempDir(), "ready")
			signals := make(chan os.Signal, 1)
			done := make(chan error, 1)
			script := "trap 'exit " + strconv.Itoa(test.exitCode) + "' " + test.trapName + "; : > \"$1\"; while :; do sleep 10; done"
			go func() {
				done <- runAsInit([]string{"/bin/sh", "-c", script, "boxr-signal-test", readyPath}, signals)
			}()

			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(readyPath); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("workload did not reach its signal handler setup")
				}
				time.Sleep(10 * time.Millisecond)
			}
			signals <- test.signal

			select {
			case err := <-done:
				var exitErr *ExitError
				if !errors.As(err, &exitErr) || exitErr.Code != test.exitCode {
					t.Fatalf("runAsInit(%s) error = %v, want exit code %d", test.name, err, test.exitCode)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("runAsInit did not reap workload after forwarding %s", test.name)
			}
		})
	}
}
