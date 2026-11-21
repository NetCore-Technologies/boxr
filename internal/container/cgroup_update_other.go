//go:build !linux

package container

import "fmt"

// CgroupResourceUpdate identifies the live resource controllers being changed.
type CgroupResourceUpdate struct {
	Memory            bool
	MemoryReservation bool
	MemorySwap        bool
	NanoCPUs          bool
	PidsLimit         bool
}

// UpdateCgroupResources is available only on Linux with cgroup v2.
func UpdateCgroupResources(pid int, options Options, update CgroupResourceUpdate) error {
	return fmt.Errorf("live cgroup resource updates require Linux with cgroup v2")
}
