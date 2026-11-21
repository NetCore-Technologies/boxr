//go:build linux

package container

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigureCgroupAtWritesRequestedLimitsAndMembership(t *testing.T) {
	root := newCgroupFixture(t)
	cleanup, err := configureCgroupAt(root, 4242, 256<<20, 0.5)
	if err != nil {
		t.Fatalf("configureCgroupAt(): %v", err)
	}
	if cleanup == nil {
		t.Fatal("configureCgroupAt() returned a nil cleanup function")
	}

	path := filepath.Join(root, "boxr", "4242")
	for name, want := range map[string]string{
		"memory.max":   "268435456",
		"cpu.max":      "50000 100000",
		"cgroup.procs": "4242",
	} {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup(): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cgroup directory still exists after cleanup (stat err: %v)", err)
	}
}

func TestConfigureCgroupAtCanApplyOnlyOneLimit(t *testing.T) {
	root := newCgroupFixture(t)
	cleanup, err := configureCgroupAt(root, 7, 0, 1.25)
	if err != nil {
		t.Fatalf("configureCgroupAt(): %v", err)
	}
	defer cleanup()
	path := filepath.Join(root, "boxr", "7")
	if _, err := os.Stat(filepath.Join(path, "memory.max")); !os.IsNotExist(err) {
		t.Fatalf("memory.max should not be written when no memory limit is set")
	}
	if got, err := os.ReadFile(filepath.Join(path, "cpu.max")); err != nil || string(got) != "125000 100000" {
		t.Fatalf("cpu.max = %q, err = %v", got, err)
	}
}

func TestConfigureCgroupAtSkipsCgroupWhenNoLimitsAreSet(t *testing.T) {
	root := t.TempDir()
	cleanup, err := configureCgroupAt(root, 7, 0, 0)
	if err != nil {
		t.Fatalf("configureCgroupAt(): %v", err)
	}
	if cleanup != nil {
		t.Fatal("expected no cleanup function when limits are disabled")
	}
	if _, err := os.Stat(filepath.Join(root, "boxr")); !os.IsNotExist(err) {
		t.Fatalf("cgroup directory unexpectedly created (stat err: %v)", err)
	}
}

func TestConfigureCgroupAtRejectsInvalidLimits(t *testing.T) {
	tests := []struct {
		name string
		pid  int
		mem  int64
		cpus float64
	}{
		{name: "zero pid", pid: 0, mem: 1},
		{name: "negative pid", pid: -1, mem: 1},
		{name: "negative memory", pid: 1, mem: -1},
		{name: "negative cpu", pid: 1, cpus: -1},
		{name: "nan cpu", pid: 1, cpus: math.NaN()},
		{name: "infinite cpu", pid: 1, cpus: math.Inf(1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := configureCgroupAt(t.TempDir(), test.pid, test.mem, test.cpus); err == nil {
				t.Fatal("expected invalid cgroup configuration to fail")
			}
		})
	}
}

func TestConfigureCgroupAtRejectsUnrepresentableCpuQuota(t *testing.T) {
	if _, err := configureCgroupAt(newCgroupFixture(t), 1, 0, 0.0000001); err == nil {
		t.Fatal("expected too-small CPU quota to fail")
	}
}

func TestConfigureCgroupAtRejectsNonDirectoryRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("file"), 0600); err != nil {
		t.Fatalf("create root fixture: %v", err)
	}
	if _, err := configureCgroupAt(root, 123, 1<<20, 0); err == nil {
		t.Fatal("expected cgroup creation to fail when the root is a regular file")
	}
}

func TestConfigureCgroupAtRejectsEmptyRootWhenLimitsAreRequested(t *testing.T) {
	if _, err := configureCgroupAt("", 123, 1<<20, 0); err == nil {
		t.Fatal("expected an empty cgroup root to be rejected")
	}
}

func newCgroupFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, "boxr")
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatalf("create cgroup fixture: %v", err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "cgroup.controllers"):       "cpu memory",
		filepath.Join(root, "cgroup.subtree_control"):   "cpu memory",
		filepath.Join(parent, "cgroup.controllers"):     "cpu memory",
		filepath.Join(parent, "cgroup.subtree_control"): "",
	} {
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatalf("write cgroup fixture %s: %v", path, err)
		}
	}
	return root
}

func TestConfigureCgroupAtEnablesOnlyRequestedControllers(t *testing.T) {
	root := newCgroupFixture(t)
	cleanup, err := configureCgroupAt(root, 88, 0, 0.5)
	if err != nil {
		t.Fatalf("configureCgroupAt(): %v", err)
	}
	defer cleanup()
	got, err := os.ReadFile(filepath.Join(root, "boxr", "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("read parent subtree control: %v", err)
	}
	if string(got) != "+cpu" {
		t.Fatalf("parent subtree control = %q, want %q", got, "+cpu")
	}
}

func TestConfigureCgroupAtRequiresRootDelegation(t *testing.T) {
	root := newCgroupFixture(t)
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("memory"), 0644); err != nil {
		t.Fatalf("remove CPU delegation: %v", err)
	}
	if _, err := configureCgroupAt(root, 99, 0, 0.5); err == nil {
		t.Fatal("expected missing CPU delegation to fail")
	}
}

func TestConfigureCgroupOptionsAtWritesExtendedResourceControls(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	options := Options{
		MemoryLimitBytes:   512 << 20,
		MemoryHighBytes:    384 << 20,
		MemoryLowBytes:     128 << 20,
		MemoryMinBytes:     64 << 20,
		MemorySwapMaxBytes: 256 << 20,
		MemoryOOMGroup:     true,
		CPUs:               1.5,
		CPUWeight:          250,
		IOWeight:           500,
		PidsLimit:          64,
	}
	cleanup, err := configureCgroupOptionsAt(root, 5150, options)
	if err != nil {
		t.Fatalf("configureCgroupOptionsAt(): %v", err)
	}
	defer cleanup()
	path := filepath.Join(root, "boxr", "5150")
	for name, want := range map[string]string{
		"memory.max": "536870912", "memory.high": "402653184", "memory.low": "134217728",
		"memory.min": "67108864", "memory.swap.max": "268435456", "memory.oom.group": "1",
		"cpu.max": "150000 100000", "cpu.weight": "250", "io.weight": "500",
		"pids.max": "64", "cgroup.procs": "5150",
	} {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestConfigureCgroupEnabledContainerEnablesAvailableStatsControllers(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory pids"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "boxr", "cgroup.controllers"), []byte("cpu memory pids"), 0644); err != nil {
		t.Fatal(err)
	}
	cleanup, err := configureCgroupOptionsAt(root, 5160, Options{CgroupEnabled: true})
	if err != nil {
		t.Fatalf("configure default Docker cgroup: %v", err)
	}
	defer cleanup()

	control, err := os.ReadFile(filepath.Join(root, "boxr", "cgroup.subtree_control"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(control), "+cpu +memory +pids"; got != want {
		t.Fatalf("optional stats controllers = %q, want %q", got, want)
	}
	child := filepath.Join(root, "boxr", "5160")
	if _, err := os.Stat(filepath.Join(child, "cgroup.procs")); err != nil {
		t.Fatalf("default Docker cgroup was not created: %v", err)
	}
}

func TestConfigureCgroupOptionsAtRejectsInvalidExtendedValues(t *testing.T) {
	for name, options := range map[string]Options{
		"negative memory high": {MemoryHighBytes: -1},
		"negative pids":        {PidsLimit: -1},
		"CPU weight too high":  {CPUWeight: 10001},
		"I/O weight too low":   {IOWeight: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configureCgroupOptionsAt(t.TempDir(), 90, options); err == nil {
				t.Fatal("expected invalid limit to fail")
			}
		})
	}
}

func newExtendedCgroupFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, "boxr")
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "cgroup.controllers"):       "cpu memory pids io",
		filepath.Join(root, "cgroup.subtree_control"):   "cpu memory pids io",
		filepath.Join(parent, "cgroup.controllers"):     "cpu memory pids io",
		filepath.Join(parent, "cgroup.subtree_control"): "",
	} {
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}
