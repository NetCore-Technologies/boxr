//go:build !linux

package container

import "fmt"

// SetCgroupFrozen is available only on Linux with cgroup v2.
func SetCgroupFrozen(pid int, frozen bool) error {
	return fmt.Errorf("container pause and resume require Linux with cgroup v2")
}
