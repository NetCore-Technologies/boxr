//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const cgroupRoot = "/sys/fs/cgroup"

func ApplyCgroup(pid int, memLimitMB int, cpuQuota int) error {
	cgroupPath := filepath.Join(cgroupRoot, "boxr", strconv.Itoa(pid))

	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("create cgroup: %w", err)
	}

	memBytes := memLimitMB * 1024 * 1024
	if err := writeFile(filepath.Join(cgroupPath, "memory.max"), strconv.Itoa(memBytes)); err != nil {
		return fmt.Errorf("set memory limit: %w", err)
	}

	cpuVal := fmt.Sprintf("%d 100000", cpuQuota*1000)
	if err := writeFile(filepath.Join(cgroupPath, "cpu.max"), cpuVal); err != nil {
		return fmt.Errorf("set cpu limit: %w", err)
	}

	if err := writeFile(filepath.Join(cgroupPath, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("add proc to cgroup: %w", err)
	}

	return nil
}

func writeFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0644)
}
