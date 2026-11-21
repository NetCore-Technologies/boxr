//go:build linux

package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateCgroupResourcesAtAppliesSelectedControllers(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	pid := 5151
	directory := filepath.Join(root, "boxr", "5151")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"memory.max": "max", "memory.low": "0", "memory.swap.max": "max",
		"cpu.max": "max 100000", "pids.max": "max",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}

	options := Options{
		MemoryLimitBytes:   256 << 20,
		MemoryLowBytes:     32 << 20,
		MemorySwapMaxBytes: 128 << 20,
		MemorySwapMaxSet:   true,
		CPUs:               0.5,
		PidsLimit:          128,
	}
	update := CgroupResourceUpdate{
		Memory: true, MemoryReservation: true, MemorySwap: true,
		NanoCPUs: true, PidsLimit: true,
	}
	if err := updateCgroupResourcesAt(root, pid, options, update); err != nil {
		t.Fatalf("updateCgroupResourcesAt(): %v", err)
	}
	for name, want := range map[string]string{
		"memory.max": "268435456", "memory.low": "33554432",
		"memory.swap.max": "134217728", "cpu.max": "50000 100000", "pids.max": "128",
	} {
		got, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestUpdateCgroupResourcesAtResetsUnlimitedValues(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	directory := filepath.Join(root, "boxr", "5152")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"memory.max": "1024", "memory.swap.max": "1024", "cpu.max": "1000 100000", "pids.max": "10",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	update := CgroupResourceUpdate{Memory: true, MemorySwap: true, NanoCPUs: true, PidsLimit: true}
	if err := updateCgroupResourcesAt(root, 5152, Options{}, update); err != nil {
		t.Fatalf("update unlimited resources: %v", err)
	}
	for name, want := range map[string]string{
		"memory.max": "max", "memory.swap.max": "max", "cpu.max": "max 100000", "pids.max": "max",
	} {
		got, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestUpdateCgroupResourcesAtValidatesBeforeWriting(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	directory := filepath.Join(root, "boxr", "5153")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "memory.max"), []byte("max"), 0644); err != nil {
		t.Fatal(err)
	}
	update := CgroupResourceUpdate{Memory: true, PidsLimit: true}
	err := updateCgroupResourcesAt(root, 5153, Options{MemoryLimitBytes: 4096}, update)
	if err == nil {
		t.Fatal("expected missing pids.max to fail")
	}
	got, readErr := os.ReadFile(filepath.Join(directory, "memory.max"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(got)) != "max" {
		t.Fatalf("memory.max changed before all controls were validated: %q", got)
	}
}

func TestUpdateCgroupResourcesAtRejectsSymlinkedContainerCgroup(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	target := filepath.Join(root, "unrelated")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "boxr", "5154")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := updateCgroupResourcesAt(root, 5154, Options{MemoryLimitBytes: 4096}, CgroupResourceUpdate{Memory: true})
	if err == nil {
		t.Fatal("symlinked cgroup directory was accepted")
	}
}

func TestConfigureCgroupEnabledCreatesAnUnboundedCgroup(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	cleanup, err := configureCgroupOptionsAt(root, 5155, Options{CgroupEnabled: true})
	if err != nil {
		t.Fatalf("configureCgroupOptionsAt(): %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected cleanup for an explicitly managed cgroup")
	}
	got, err := os.ReadFile(filepath.Join(root, "boxr", "5155", "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "5155" {
		t.Fatalf("cgroup.procs = %q, want 5155", got)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCgroupResourcesAtCanSetAnExplicitZeroSwapLimit(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	directory := filepath.Join(root, "boxr", "5156")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "memory.swap.max"), []byte("max"), 0644); err != nil {
		t.Fatal(err)
	}
	options := Options{MemoryLimitBytes: 1 << 20, MemorySwapMaxSet: true}
	if err := updateCgroupResourcesAt(root, 5156, options, CgroupResourceUpdate{MemorySwap: true}); err != nil {
		t.Fatalf("set explicit zero swap: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(directory, "memory.swap.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "0" {
		t.Fatalf("memory.swap.max = %q, want 0", got)
	}
}
