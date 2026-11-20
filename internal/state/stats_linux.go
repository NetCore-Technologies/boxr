//go:build linux

package state

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func ReadStats(pid int) (Stats, error) {
	var result Stats
	if pid <= 0 {
		return result, fmt.Errorf("container has no active host PID")
	}
	path := filepath.Join("/sys/fs/cgroup/boxr", strconv.Itoa(pid))
	if _, err := os.Stat(path); err != nil {
		return result, fmt.Errorf("container cgroup is unavailable: %w", err)
	}
	result.PID = pid
	readInt := func(name string) (int64, error) {
		data, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
	if value, err := readInt("memory.current"); err == nil {
		result.MemoryCurrent = value
	}
	if data, err := os.ReadFile(filepath.Join(path, "memory.max")); err == nil {
		result.MemoryMax = strings.TrimSpace(string(data))
	}
	if value, err := readInt("pids.current"); err == nil {
		result.PIDsCurrent = value
	}
	if file, err := os.Open(filepath.Join(path, "cpu.stat")); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) != 2 {
				continue
			}
			value, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr != nil {
				continue
			}
			switch fields[0] {
			case "usage_usec":
				result.CPUUsageUsec = value
			case "user_usec":
				result.CPUUserUsec = value
			case "system_usec":
				result.CPUSystemUsec = value
			}
		}
		_ = file.Close()
	}
	if file, err := os.Open(filepath.Join(path, "io.stat")); err == nil {
		result.IOCounters = make(map[string]string)
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) > 1 {
				result.IOCounters[fields[0]] = strings.Join(fields[1:], " ")
			}
		}
		_ = file.Close()
	}
	entries, err := os.ReadDir(path)
	if err == nil {
		for _, entry := range entries {
			result.AvailableFiles = append(result.AvailableFiles, entry.Name())
		}
	}
	if result.MemoryCurrent == 0 && result.MemoryMax == "" && result.PIDsCurrent == 0 && result.CPUUsageUsec == 0 && len(result.IOCounters) == 0 {
		return result, fmt.Errorf("cgroup statistics files are unavailable or unreadable")
	}
	return result, nil
}
