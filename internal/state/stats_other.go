//go:build !linux

package state

import "fmt"

type Stats struct {
	PID int `json:"pid"`
}

func ReadStats(pid int) (Stats, error) {
	return Stats{}, fmt.Errorf("cgroup statistics require Linux")
}
