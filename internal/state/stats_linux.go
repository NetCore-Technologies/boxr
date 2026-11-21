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
	cgroupPath := filepath.Join("/sys/fs/cgroup/boxr", strconv.Itoa(pid))
	info, err := os.Stat(cgroupPath)
	if err != nil {
		return result, fmt.Errorf("container cgroup is unavailable: %w", err)
	}
	if !info.IsDir() {
		return result, fmt.Errorf("container cgroup path is not a directory")
	}
	result.PID = pid
	readInt := func(name string) (int64, error) {
		data, err := os.ReadFile(filepath.Join(cgroupPath, name))
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
	if value, err := readInt("memory.current"); err == nil {
		result.MemoryCurrent = value
	}
	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.max")); err == nil {
		result.MemoryMax = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.stat")); err == nil {
		result.MemoryStats = parseCgroupKeyValues(string(data))
	}
	if value, err := readInt("memory.peak"); err == nil {
		if result.MemoryStats == nil {
			result.MemoryStats = make(map[string]int64)
		}
		result.MemoryStats["max_usage"] = value
	}
	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.events")); err == nil {
		if result.MemoryStats == nil {
			result.MemoryStats = make(map[string]int64)
		}
		for key, value := range parseCgroupKeyValues(string(data)) {
			result.MemoryStats[key] = value
		}
	}
	if value, err := readInt("pids.current"); err == nil {
		result.PIDsCurrent = value
	}
	if value, err := readInt("pids.max"); err == nil {
		result.PIDsMax = value
	}
	if file, err := os.Open(filepath.Join(cgroupPath, "cpu.stat")); err == nil {
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
			case "nr_periods":
				result.CPUPeriods = value
			case "nr_throttled":
				result.CPUThrottledPeriods = value
			case "throttled_usec":
				result.CPUThrottledUsec = value
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return result, fmt.Errorf("read container cpu.stat: %w", scanErr)
		}
		if closeErr != nil {
			return result, fmt.Errorf("close container cpu.stat: %w", closeErr)
		}
	}
	if file, err := os.Open(filepath.Join(cgroupPath, "io.stat")); err == nil {
		result.IOCounters = make(map[string]string)
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) > 1 {
				result.IOCounters[fields[0]] = strings.Join(fields[1:], " ")
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return result, fmt.Errorf("read container io.stat: %w", scanErr)
		}
		if closeErr != nil {
			return result, fmt.Errorf("close container io.stat: %w", closeErr)
		}
	}
	result.SystemCPUUsageNS, _ = readHostCPUUsageNS()
	result.SystemMemoryTotalBytes, _ = readHostMemoryTotalBytes()
	result.Networks, _ = readProcessNetworkCounters(pid)
	entries, err := os.ReadDir(cgroupPath)
	if err == nil {
		for _, entry := range entries {
			result.AvailableFiles = append(result.AvailableFiles, entry.Name())
		}
	}
	if result.MemoryCurrent == 0 && result.MemoryMax == "" && result.PIDsCurrent == 0 &&
		result.CPUUsageUsec == 0 && len(result.IOCounters) == 0 && len(result.Networks) == 0 {
		return result, fmt.Errorf("cgroup statistics files are unavailable or unreadable")
	}
	return result, nil
}

func parseCgroupKeyValues(data string) map[string]int64 {
	values := make(map[string]int64)
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err == nil {
			values[fields[0]] = value
		}
	}
	return values
}

func readHostCPUUsageNS() (int64, error) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("/proc/stat has no aggregate CPU line")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, fmt.Errorf("/proc/stat aggregate CPU line is malformed")
	}
	var ticks int64
	if len(fields) > 9 {
		fields = fields[:9]
	}
	for _, field := range fields[1:] {
		value, parseErr := strconv.ParseInt(field, 10, 64)
		if parseErr != nil {
			break
		}
		if value > 0 && ticks > int64(^uint64(0)>>1)-value {
			return 0, fmt.Errorf("host CPU tick counter overflow")
		}
		ticks += value
	}
	// Linux /proc/stat exports USER_HZ clock ticks (100 ticks per second
	// on supported Boxr Linux platforms); Docker expects nanoseconds.
	if ticks > int64(^uint64(0)>>1)/10_000_000 {
		return 0, fmt.Errorf("host CPU nanosecond counter overflow")
	}
	return ticks * 10_000_000, scanner.Err()
}

func readHostMemoryTotalBytes() (int64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kibibytes, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr != nil || kibibytes < 0 || kibibytes > int64(^uint64(0)>>1)/1024 {
				return 0, fmt.Errorf("invalid MemTotal in /proc/meminfo")
			}
			return kibibytes * 1024, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("/proc/meminfo has no MemTotal entry")
}

func readProcessNetworkCounters(pid int) (map[string]NetworkCounters, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "net", "dev")
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	result := make(map[string]NetworkCounters)
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if lineNumber <= 2 {
			continue
		}
		name, values, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(values)
		if len(fields) < 16 {
			continue
		}
		counters := make([]int64, 8)
		indices := []int{0, 1, 2, 3, 8, 9, 10, 11}
		valid := true
		for index, fieldIndex := range indices {
			value, parseErr := strconv.ParseInt(fields[fieldIndex], 10, 64)
			if parseErr != nil {
				valid = false
				break
			}
			counters[index] = value
		}
		if !valid {
			continue
		}
		interfaceName := strings.TrimSpace(name)
		if interfaceName == "" {
			continue
		}
		result[interfaceName] = NetworkCounters{
			RxBytes: counters[0], RxPackets: counters[1], RxErrors: counters[2], RxDropped: counters[3],
			TxBytes: counters[4], TxPackets: counters[5], TxErrors: counters[6], TxDropped: counters[7],
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	return result, nil
}
