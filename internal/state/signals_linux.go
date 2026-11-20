//go:build linux

package state

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func Signal(query, name string) error {
	record, err := Get(query)
	if err != nil {
		return err
	}
	if record.PID <= 0 {
		return fmt.Errorf("container %s has no host PID", record.ID)
	}
	signal, err := parseSignal(name)
	if err != nil {
		return err
	}
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return err
	}
	if err := process.Signal(signal); err != nil {
		return fmt.Errorf("send %s to container %s (PID %d): %w", strings.ToUpper(name), record.ID, record.PID, err)
	}
	return nil
}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func parseSignal(name string) (os.Signal, error) {
	switch strings.ToUpper(strings.TrimPrefix(name, "SIG")) {
	case "HUP":
		return syscall.SIGHUP, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "STOP":
		return syscall.SIGSTOP, nil
	case "CONT":
		return syscall.SIGCONT, nil
	default:
		return nil, fmt.Errorf("unsupported signal %q (use HUP, INT, QUIT, TERM, KILL, USR1, USR2, STOP, or CONT)", name)
	}
}
