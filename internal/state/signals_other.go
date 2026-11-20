//go:build !linux

package state

import "fmt"

func Signal(query, name string) error {
	return fmt.Errorf("sending container signals is supported only on Linux")
}

func ProcessAlive(pid int) bool { return false }
