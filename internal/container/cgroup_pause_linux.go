//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cgroupFreezeTimeout = 5 * time.Second
	cgroupFreezePoll    = 10 * time.Millisecond
)

// SetCgroupFrozen pauses or resumes every process in a Boxr-managed cgroup v2
// subtree. The call returns only after the kernel reports the requested state.
func SetCgroupFrozen(pid int, frozen bool) error {
	return setCgroupFrozenAt(cgroupRoot, pid, frozen, cgroupFreezeTimeout, cgroupFreezePoll)
}

func setCgroupFrozenAt(root string, pid int, frozen bool, timeout, poll time.Duration) error {
	if pid <= 1 {
		return fmt.Errorf("container PID must be greater than 1")
	}
	if timeout <= 0 || poll <= 0 {
		return fmt.Errorf("cgroup freezer timeout and polling interval must be positive")
	}
	parent := filepath.Join(root, "boxr")
	directory := filepath.Join(parent, strconv.Itoa(pid))
	if err := verifyCgroupDirectory(parent); err != nil {
		return fmt.Errorf("verify Boxr cgroup parent: %w", err)
	}
	if err := verifyCgroupDirectory(directory); err != nil {
		return fmt.Errorf("verify container cgroup: %w", err)
	}

	freezePath := filepath.Join(directory, "cgroup.freeze")
	eventsPath := filepath.Join(directory, "cgroup.events")
	for _, path := range []string{freezePath, eventsPath} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect cgroup freezer control %s: %w", filepath.Base(path), err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("cgroup freezer control %s is not a regular file", filepath.Base(path))
		}
	}
	value := "0"
	if frozen {
		value = "1"
	}
	if err := writeCgroupFile(freezePath, value); err != nil {
		return fmt.Errorf("write cgroup.freeze: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(eventsPath)
		if err != nil {
			return fmt.Errorf("read cgroup.events: %w", err)
		}
		observed, found, err := parseCgroupFrozenEvent(data)
		if err != nil {
			return err
		}
		if found && observed == frozen {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("cgroup freezer did not reach frozen=%t before timeout", frozen)
		}
		time.Sleep(poll)
	}
}

func parseCgroupFrozenEvent(data []byte) (bool, bool, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "frozen" {
			continue
		}
		switch fields[1] {
		case "0":
			return false, true, nil
		case "1":
			return true, true, nil
		default:
			return false, false, fmt.Errorf("invalid cgroup.events frozen value %q", fields[1])
		}
	}
	return false, false, nil
}
