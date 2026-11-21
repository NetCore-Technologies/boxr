package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func runPause(args []string, stdout, stderr io.Writer) int {
	return runPauseCommand(args, true, stdout, stderr)
}

func runUnpause(args []string, stdout, stderr io.Writer) int {
	return runPauseCommand(args, false, stdout, stderr)
}

func runPauseCommand(args []string, pause bool, stdout, stderr io.Writer) int {
	action, targetStatus := "pause", "paused"
	if !pause {
		action, targetStatus = "unpause", "running"
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, "Usage: boxr %s ID|NAME [...]
", action)
		return 2
	}
	failed := false
	for _, query := range args {
		record, err := state.Get(query)
		if err != nil {
			fmt.Fprintf(stderr, "boxr %s: %s: %v
", action, query, err)
			failed = true
			continue
		}
		if record.Status == targetStatus {
			fmt.Fprintf(stdout, "%s is already %s
", record.ID, targetStatus)
			continue
		}
		if err := transitionContainerPause(record, pause, container.SetCgroupFrozen); err != nil {
			fmt.Fprintf(stderr, "boxr %s: %s: %v
", action, record.ID, err)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "%sd %s
", action, record.ID)
	}
	if failed {
		return 1
	}
	return 0
}

// transitionContainerPause changes the kernel freezer before persisting the
// matching state. If process identity or state persistence changes meanwhile,
// it restores the previous freezer state rather than leaving metadata and the
// actual workload out of sync.
func transitionContainerPause(record state.Record, pause bool, setFrozen func(int, bool) error) error {
	action, expectedStatus := "pause", "running"
	if !pause {
		action, expectedStatus = "unpause", "paused"
	}
	if record.Status != expectedStatus {
		return fmt.Errorf("container must be %s before %s (current state: %s)", expectedStatus, action, record.Status)
	}
	if record.PID <= 1 || record.ProcessStartTicks == 0 {
		return fmt.Errorf("container has no verifiable running process identity")
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		return fmt.Errorf("refuse %s for an unverified process: %w", action, err)
	}
	if setFrozen == nil {
		return fmt.Errorf("cgroup pause and resume are unavailable")
	}
	if err := setFrozen(record.PID, pause); err != nil {
		return fmt.Errorf("apply cgroup %s: %w", action, err)
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		rollbackErr := setFrozen(record.PID, !pause)
		return errors.Join(fmt.Errorf("process identity changed during %s: %w", action, err), rollbackErr)
	}
	if err := state.SetPaused(record.ID, record.PID, record.ProcessStartTicks, pause); err != nil {
		rollbackErr := setFrozen(record.PID, !pause)
		return errors.Join(fmt.Errorf("persist container %s state: %w", action, err), rollbackErr)
	}
	return nil
}
