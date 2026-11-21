package state

import (
	"errors"
	"fmt"
	"time"
)

// SetPaused persists a pause transition only while the original process is
// still the recorded running process. The caller freezes or thaws the process
// cgroup before calling this function.
func SetPaused(id string, expectedPID int, expectedStartTicks uint64, paused bool) error {
	if expectedPID <= 1 || expectedStartTicks == 0 {
		return fmt.Errorf("a verified container PID and process start time are required")
	}
	storeMu.Lock()
	defer storeMu.Unlock()

	record, err := findLocked(id)
	if err != nil {
		return err
	}
	expectedStatus, newStatus, action := "running", "paused", "pause"
	if !paused {
		expectedStatus, newStatus, action = "paused", "running", "unpause"
	}
	if record.Status != expectedStatus {
		return fmt.Errorf("container %s changed state during %s: expected %s, found %s", record.ID, action, expectedStatus, record.Status)
	}
	if record.StopRequested {
		return fmt.Errorf("container %s has a pending stop request", record.ID)
	}
	if record.PID != expectedPID || record.ProcessStartTicks != expectedStartTicks {
		return fmt.Errorf("container %s process identity changed during %s", record.ID, action)
	}

	previous := record
	record.Status = newStatus
	if err := saveRecord(record); err != nil {
		return fmt.Errorf("persist container %s state %s: %w", action, newStatus, err)
	}
	event := Event{Time: time.Now().UTC(), Action: action, ID: record.ID, Name: record.Name, PID: record.PID}
	if err := appendEvent(event); err != nil {
		rollbackErr := saveRecord(previous)
		return errors.Join(fmt.Errorf("record container %s event: %w", action, err), rollbackErr)
	}
	return nil
}
