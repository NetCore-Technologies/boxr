//go:build !linux

package state

import "fmt"

func ReadStats(pid int) (Stats, error) {
	return Stats{}, fmt.Errorf("cgroup statistics require Linux")
}
