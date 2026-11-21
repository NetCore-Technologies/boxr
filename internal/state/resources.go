package state

import (
	"fmt"

	"github.com/manit6752025/boxr/internal/container"
)

// UpdateRunningOptions persists resource settings only while the same
// container process remains the active recorded process. The caller must
// verify the kernel process identity and apply the cgroup change first.
func UpdateRunningOptions(id string, expectedPID int, expectedStartTicks uint64, options container.Options) (Record, error) {
	if expectedPID <= 1 || expectedStartTicks == 0 {
		return Record{}, fmt.Errorf("a verified container PID and process start time are required")
	}

	storeMu.Lock()
	defer storeMu.Unlock()

	record, err := findLocked(id)
	if err != nil {
		return Record{}, err
	}
	if record.Status != "running" || record.StopRequested {
		return Record{}, fmt.Errorf("container %s is no longer running", record.ID)
	}
	if record.PID != expectedPID || record.ProcessStartTicks != expectedStartTicks {
		return Record{}, fmt.Errorf("container %s process identity changed during resource update", record.ID)
	}

	record.Options = options
	if err := saveRecord(record); err != nil {
		return Record{}, fmt.Errorf("persist updated container resources: %w", err)
	}
	return record, nil
}
