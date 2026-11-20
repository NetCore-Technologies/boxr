//go:build linux

package state

import (
	"os/exec"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
)

func TestSignalStopsRecordedProcess(t *testing.T) {
	useTempState(t)
	command := exec.Command("sh", "-c", "exec sleep 30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	record, err := Create("signal-test", []string{"sleep", "30"}, container.Options{}, nil)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := Start(record.ID, command.Process.Pid); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if !ProcessAlive(command.Process.Pid) {
		t.Fatal("started test process is not reported alive")
	}
	if err := Signal(record.ID, "SIGTERM"); err != nil {
		t.Fatal(err)
	}
	waitErr := command.Wait()
	if waitErr == nil {
		t.Fatal("SIGTERM did not terminate the test process")
	}
	if ProcessAlive(command.Process.Pid) {
		t.Fatal("terminated process is still reported alive")
	}
	if err := Finish(record.ID, waitErr); err != nil {
		t.Fatal(err)
	}
	finished, err := Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.ExitCode == nil || *finished.ExitCode != 143 {
		var exitErr *exec.ExitError
		if ok := (waitErr != nil); ok {
			exitErr, _ = waitErr.(*exec.ExitError)
		}
		t.Fatalf("signal exit code = %#v, want 143; waitErr=%T %v; exitErr=%#v", finished.ExitCode, waitErr, waitErr, exitErr)
	}
}

func TestSignalRejectsUnsupportedName(t *testing.T) {
	if _, err := parseSignal("NOT_A_SIGNAL"); err == nil {
		t.Fatal("unsupported signal accepted")
	}
}
