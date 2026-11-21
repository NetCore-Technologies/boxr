//go:build linux

package container

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgroupRoot = "/sys/fs/cgroup"
const cgroupPeriodMicros = int64(100000)

func applyCgroup(pid int, options Options) (func() error, error) {
	return configureCgroupOptionsAt(cgroupRoot, pid, options)
}

// configureCgroupAt is separated from the system cgroup root so the cgroup
// configuration and rollback behaviour can be tested with a temporary folder.
func configureCgroupAt(root string, pid int, memoryLimitBytes int64, cpus float64) (func() error, error) {
	return configureCgroupOptionsAt(root, pid, Options{MemoryLimitBytes: memoryLimitBytes, CPUs: cpus})
}

func configureCgroupOptionsAt(root string, pid int, options Options) (func() error, error) {
	memoryLimitBytes := options.MemoryLimitBytes
	cpus := options.CPUs
	if pid <= 0 {
		return nil, fmt.Errorf("PID must be positive")
	}
	if memoryLimitBytes < 0 || options.MemoryHighBytes < 0 || options.MemoryLowBytes < 0 || options.MemoryMinBytes < 0 || options.MemorySwapMaxBytes < 0 {
		return nil, fmt.Errorf("memory limits cannot be negative")
	}
	if options.PidsLimit < 0 {
		return nil, fmt.Errorf("PID limit cannot be negative")
	}
	if options.CPUWeight != 0 && (options.CPUWeight < 1 || options.CPUWeight > 10000) {
		return nil, fmt.Errorf("CPU weight must be between 1 and 10000")
	}
	if options.IOWeight != 0 && (options.IOWeight < 1 || options.IOWeight > 10000) {
		return nil, fmt.Errorf("I/O weight must be between 1 and 10000")
	}
	if cpus < 0 || math.IsNaN(cpus) || math.IsInf(cpus, 0) {
		return nil, fmt.Errorf("CPU limit must be a finite positive number")
	}
	if memoryLimitBytes == 0 && options.MemoryHighBytes == 0 && options.MemoryLowBytes == 0 && options.MemoryMinBytes == 0 && options.MemorySwapMaxBytes == 0 && cpus == 0 && options.CPUWeight == 0 && options.IOWeight == 0 && options.PidsLimit == 0 && !options.MemoryOOMGroup && !options.MemorySwapMaxSet && !options.CgroupEnabled {
		return nil, nil
	}
	if root == "" {
		return nil, fmt.Errorf("cgroup root cannot be empty")
	}

	parent := filepath.Join(root, "boxr")
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, fmt.Errorf("create cgroup parent %s: %w", parent, err)
	}
	controllers := make([]string, 0, 4)
	if memoryLimitBytes > 0 || options.MemoryHighBytes > 0 || options.MemoryLowBytes > 0 || options.MemoryMinBytes > 0 || options.MemorySwapMaxBytes > 0 || options.MemorySwapMaxSet || options.MemoryOOMGroup {
		controllers = append(controllers, "memory")
	}
	if cpus > 0 || options.CPUWeight > 0 {
		controllers = append(controllers, "cpu")
	}
	if options.PidsLimit > 0 {
		controllers = append(controllers, "pids")
	}
	if options.IOWeight > 0 {
		controllers = append(controllers, "io")
	}
	if err := enableControllersForChildren(root, parent, controllers); err != nil {
		return nil, fmt.Errorf("prepare cgroup controllers: %w", err)
	}
	if options.CgroupEnabled {
		// Docker-compatible Engine API containers always create a cgroup, even
		// when no hard limit was requested. Enable any additional controllers
		// already delegated to Boxr so `docker stats` can report useful
		// CPU, memory, I/O and PID counters for default containers too. Missing
		// optional controllers do not make an otherwise valid workload fail.
		enableAvailableOptionalControllers(root, parent, []string{"cpu", "memory", "io", "pids"})
	}

	path := filepath.Join(parent, strconv.Itoa(pid))
	if err := os.Mkdir(path, 0755); err != nil {
		return nil, fmt.Errorf("create %s (a stale cgroup with this PID may already exist): %w", path, err)
	}
	cleanup := func() error { return removeCgroupDirectory(path) }
	rollback := func(err error) (func() error, error) {
		_ = cleanup()
		return nil, err
	}

	if memoryLimitBytes > 0 {
		if err := writeCgroupFile(filepath.Join(path, "memory.max"), strconv.FormatInt(memoryLimitBytes, 10)); err != nil {
			return rollback(fmt.Errorf("set memory.max: %w", err))
		}
	}
	if options.MemoryHighBytes > 0 {
		if err := writeCgroupFile(filepath.Join(path, "memory.high"), strconv.FormatInt(options.MemoryHighBytes, 10)); err != nil {
			return rollback(fmt.Errorf("set memory.high: %w", err))
		}
	}
	if options.MemoryLowBytes > 0 {
		if err := writeCgroupFile(filepath.Join(path, "memory.low"), strconv.FormatInt(options.MemoryLowBytes, 10)); err != nil {
			return rollback(fmt.Errorf("set memory.low: %w", err))
		}
	}
	if options.MemoryMinBytes > 0 {
		if err := writeCgroupFile(filepath.Join(path, "memory.min"), strconv.FormatInt(options.MemoryMinBytes, 10)); err != nil {
			return rollback(fmt.Errorf("set memory.min: %w", err))
		}
	}
	if options.MemorySwapMaxSet || options.MemorySwapMaxBytes > 0 {
		if err := writeCgroupFile(filepath.Join(path, "memory.swap.max"), strconv.FormatInt(options.MemorySwapMaxBytes, 10)); err != nil {
			return rollback(fmt.Errorf("set memory.swap.max: %w", err))
		}
	}
	if options.MemoryOOMGroup {
		if err := writeCgroupFile(filepath.Join(path, "memory.oom.group"), "1"); err != nil {
			return rollback(fmt.Errorf("set memory.oom.group: %w", err))
		}
	}
	if cpus > 0 {
		quotaFloat := cpus * float64(cgroupPeriodMicros)
		if math.IsInf(quotaFloat, 0) || quotaFloat >= float64(math.MaxInt64) {
			return rollback(fmt.Errorf("CPU limit is too large"))
		}
		quota := int64(math.Round(quotaFloat))
		if quota < 1 {
			return rollback(fmt.Errorf("CPU limit is too small to represent"))
		}
		value := fmt.Sprintf("%d %d", quota, cgroupPeriodMicros)
		if err := writeCgroupFile(filepath.Join(path, "cpu.max"), value); err != nil {
			return rollback(fmt.Errorf("set cpu.max: %w", err))
		}
	}
	if options.CPUWeight > 0 {
		if err := writeCgroupFile(filepath.Join(path, "cpu.weight"), strconv.Itoa(options.CPUWeight)); err != nil {
			return rollback(fmt.Errorf("set cpu.weight: %w", err))
		}
	}
	if options.IOWeight > 0 {
		if err := writeCgroupFile(filepath.Join(path, "io.weight"), strconv.Itoa(options.IOWeight)); err != nil {
			return rollback(fmt.Errorf("set io.weight: %w", err))
		}
	}
	if options.PidsLimit > 0 {
		if err := writeCgroupFile(filepath.Join(path, "pids.max"), strconv.FormatInt(options.PidsLimit, 10)); err != nil {
			return rollback(fmt.Errorf("set pids.max: %w", err))
		}
	}
	if err := writeCgroupFile(filepath.Join(path, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return rollback(fmt.Errorf("attach PID to cgroup.procs: %w", err))
	}
	return cleanup, nil
}

// enableControllersForChildren verifies the host delegation and enables the
// requested controllers in boxr's empty parent cgroup before creating a child.
// cgroup v2 requires controllers to be enabled at every parent level; checking
// only the mount root is not enough for /sys/fs/cgroup/boxr/<pid>.
func enableAvailableOptionalControllers(root, parent string, requested []string) {
	rootAvailable, err := readControllerSet(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return
	}
	rootEnabled, err := readControllerSet(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return
	}
	parentAvailable, err := readControllerSet(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return
	}
	parentEnabled, err := readControllerSet(filepath.Join(parent, "cgroup.subtree_control"))
	if err != nil {
		return
	}
	available := make([]string, 0, len(requested))
	for _, controller := range requested {
		if rootAvailable[controller] && rootEnabled[controller] && parentAvailable[controller] && !parentEnabled[controller] {
			available = append(available, controller)
		}
	}
	if len(available) > 0 {
		// This is best-effort telemetry enablement. Explicitly requested
		// resource controllers above remain fail-closed and are never skipped.
		_ = enableControllersForChildren(root, parent, available)
	}
}

func enableControllersForChildren(root, parent string, requested []string) error {
	if len(requested) == 0 {
		return nil
	}
	rootAvailable, err := readControllerSet(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read root cgroup.controllers: %w", err)
	}
	rootEnabled, err := readControllerSet(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("read root cgroup.subtree_control: %w", err)
	}
	parentAvailable, err := readControllerSet(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read %s/cgroup.controllers: %w", parent, err)
	}
	parentEnabled, err := readControllerSet(filepath.Join(parent, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("read %s/cgroup.subtree_control: %w", parent, err)
	}

	pending := make([]string, 0, len(requested))
	for _, controller := range requested {
		if !rootAvailable[controller] {
			return fmt.Errorf("%s controller is not available at the cgroup root", controller)
		}
		if !rootEnabled[controller] {
			return fmt.Errorf("%s controller is not delegated by the cgroup root; ask the host administrator to enable it", controller)
		}
		if !parentAvailable[controller] {
			return fmt.Errorf("%s controller is not delegated to the boxr cgroup", controller)
		}
		if !parentEnabled[controller] {
			pending = append(pending, "+"+controller)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	controlPath := filepath.Join(parent, "cgroup.subtree_control")
	if err := writeCgroupFile(controlPath, strings.Join(pending, " ")); err != nil {
		// Another boxr process may have enabled the same controllers after our
		// read. Re-check before treating a concurrent update as a real failure.
		refreshed, readErr := readControllerSet(controlPath)
		if readErr == nil {
			allEnabled := true
			for _, controller := range pending {
				if !refreshed[strings.TrimLeft(controller, "+- ")] {
					allEnabled = false
					break
				}
			}
			if allEnabled {
				return nil
			}
		}
		return fmt.Errorf("enable %s for boxr child cgroups: %w", strings.Join(requested, ", "), err)
	}
	return nil
}

func readControllerSet(path string) (map[string]bool, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	controllers := make(map[string]bool)
	for _, controller := range strings.Fields(string(contents)) {
		controllers[strings.TrimLeft(controller, "+- ")] = true
	}
	return controllers, nil
}

func writeCgroupFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0644)
}

func removeCgroupDirectory(path string) error {
	if err := os.Remove(path); err == nil {
		return nil
	}
	// Real cgroup control files are virtual and cannot be unlinked. Removing
	// regular-file test doubles first keeps cleanup testable on ordinary filesystems.
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(path, entry.Name()))
	}
	return os.Remove(path)
}
