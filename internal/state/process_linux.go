//go:build linux

package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processStartTicks reads field 22 of /proc/PID/stat. Parsing after the final
// ')' matters because the parenthesized command name can itself contain ')'.
func processStartTicks(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("PID must be positive")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseProcessStartTicks(string(data))
}

func parseProcessStartTicks(stat string) (uint64, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 || end+1 >= len(stat) {
		return 0, fmt.Errorf("malformed /proc stat record")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) == 0 || fields[0] == "Z" || fields[0] == "X" {
		return 0, fmt.Errorf("process has already exited")
	}
	// Remaining fields begin at field 3 (state); starttime is field 22.
	if len(fields) <= 19 {
		return 0, fmt.Errorf("short /proc stat record")
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return ticks, nil
}

// ReadProcessStartTicks returns the kernel start time used to detect PID reuse.
func ReadProcessStartTicks(pid int) (uint64, error) {
	return processStartTicks(pid)
}

// VerifyProcessIdentity rejects a record whose host PID now refers to another
// process. Zero ticks are accepted for older records without this metadata.
func VerifyProcessIdentity(record Record) error {
	if record.PID <= 1 {
		return fmt.Errorf("host PID must be greater than 1")
	}
	if record.ProcessStartTicks == 0 {
		return nil
	}
	current, err := processStartTicks(record.PID)
	if err != nil {
		return fmt.Errorf("read host PID %d identity: %w", record.PID, err)
	}
	if current != record.ProcessStartTicks {
		return fmt.Errorf("host PID %d was reused by a different process", record.PID)
	}
	return nil
}
