//go:build linux

package container

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupResourceUpdate identifies the live resource controllers being changed.
// Fields not selected here are left untouched in the kernel.
type CgroupResourceUpdate struct {
	Memory            bool
	MemoryReservation bool
	MemorySwap        bool
	NanoCPUs          bool
	PidsLimit         bool
}

// UpdateCgroupResources applies Docker-compatible resource changes to a
// container's existing cgroup v2 directory. It never creates a new cgroup:
// every Engine API container is placed in one before its startup barrier opens.
func UpdateCgroupResources(pid int, options Options, update CgroupResourceUpdate) error {
	return updateCgroupResourcesAt(cgroupRoot, pid, options, update)
}

func updateCgroupResourcesAt(root string, pid int, options Options, update CgroupResourceUpdate) error {
	if pid <= 1 {
		return fmt.Errorf("container PID must be greater than 1")
	}
	if !update.Memory && !update.MemoryReservation && !update.MemorySwap && !update.NanoCPUs && !update.PidsLimit {
		return fmt.Errorf("at least one supported cgroup resource must be selected")
	}
	if options.MemoryLimitBytes < 0 || options.MemoryLowBytes < 0 || options.MemorySwapMaxBytes < 0 {
		return fmt.Errorf("memory limits cannot be negative")
	}
	if options.PidsLimit < 0 {
		return fmt.Errorf("PID limit cannot be negative")
	}
	if options.CPUs < 0 || math.IsNaN(options.CPUs) || math.IsInf(options.CPUs, 0) {
		return fmt.Errorf("CPU limit must be finite and non-negative")
	}

	parent := filepath.Join(root, "boxr")
	path := filepath.Join(parent, strconv.Itoa(pid))
	if err := verifyCgroupDirectory(parent); err != nil {
		return fmt.Errorf("verify Boxr cgroup parent: %w", err)
	}
	if err := verifyCgroupDirectory(path); err != nil {
		return fmt.Errorf("verify container cgroup: %w", err)
	}

	controllers := make([]string, 0, 3)
	changes := make([]cgroupResourceChange, 0, 5)
	if update.Memory || update.MemoryReservation || update.MemorySwap {
		controllers = append(controllers, "memory")
	}
	if update.NanoCPUs {
		controllers = append(controllers, "cpu")
	}
	if update.PidsLimit {
		controllers = append(controllers, "pids")
	}
	if update.Memory {
		value := "max"
		if options.MemoryLimitBytes > 0 {
			value = strconv.FormatInt(options.MemoryLimitBytes, 10)
		}
		changes = append(changes, cgroupResourceChange{name: "memory.max", value: value})
	}
	if update.MemoryReservation {
		changes = append(changes, cgroupResourceChange{
			name: "memory.low", value: strconv.FormatInt(options.MemoryLowBytes, 10),
		})
	}
	if update.MemorySwap {
		value := "max"
		if options.MemorySwapMaxSet || options.MemorySwapMaxBytes > 0 {
			value = strconv.FormatInt(options.MemorySwapMaxBytes, 10)
		}
		changes = append(changes, cgroupResourceChange{name: "memory.swap.max", value: value})
	}
	if update.NanoCPUs {
		value := "max " + strconv.FormatInt(cgroupPeriodMicros, 10)
		if options.CPUs > 0 {
			quotaFloat := options.CPUs * float64(cgroupPeriodMicros)
			if math.IsInf(quotaFloat, 0) || quotaFloat >= float64(math.MaxInt64) {
				return fmt.Errorf("CPU limit is too large")
			}
			quota := int64(math.Round(quotaFloat))
			if quota < 1 {
				return fmt.Errorf("CPU limit is too small to represent")
			}
			value = strconv.FormatInt(quota, 10) + " " + strconv.FormatInt(cgroupPeriodMicros, 10)
		}
		changes = append(changes, cgroupResourceChange{name: "cpu.max", value: value})
	}
	if update.PidsLimit {
		value := "max"
		if options.PidsLimit > 0 {
			value = strconv.FormatInt(options.PidsLimit, 10)
		}
		changes = append(changes, cgroupResourceChange{name: "pids.max", value: value})
	}

	// Enabling a controller is a parent-cgroup operation and must happen before
	// reading the corresponding control file from the container's child cgroup.
	if err := enableControllersForChildren(root, parent, controllers); err != nil {
		return fmt.Errorf("enable live resource controllers: %w", err)
	}

	oldValues := make([]string, len(changes))
	for index, change := range changes {
		controlPath := filepath.Join(path, change.name)
		info, err := os.Lstat(controlPath)
		if err != nil {
			return fmt.Errorf("inspect cgroup control %s: %w", change.name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("cgroup control %s is not a regular file", change.name)
		}
		data, err := os.ReadFile(controlPath)
		if err != nil {
			return fmt.Errorf("read current cgroup control %s: %w", change.name, err)
		}
		oldValues[index] = strings.TrimSpace(string(data))
	}

	written := 0
	for _, change := range changes {
		controlPath := filepath.Join(path, change.name)
		if err := writeCgroupFile(controlPath, change.value); err != nil {
			rollbackErr := rollbackCgroupResourceChanges(path, changes[:written], oldValues[:written])
			return errors.Join(fmt.Errorf("update cgroup control %s: %w", change.name, err), rollbackErr)
		}
		written++
	}
	return nil
}

type cgroupResourceChange struct {
	name  string
	value string
}

func verifyCgroupDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a real cgroup directory", path)
	}
	return nil
}

func rollbackCgroupResourceChanges(directory string, changes []cgroupResourceChange, oldValues []string) error {
	var rollbackErr error
	for index := len(changes) - 1; index >= 0; index-- {
		path := filepath.Join(directory, changes[index].name)
		if err := writeCgroupFile(path, oldValues[index]); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore cgroup control %s: %w", changes[index].name, err))
		}
	}
	return rollbackErr
}
