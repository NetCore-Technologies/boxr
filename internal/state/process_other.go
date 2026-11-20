//go:build !linux

package state

func processStartTicks(pid int) (uint64, error) { return 0, nil }

func ReadProcessStartTicks(pid int) (uint64, error) { return 0, nil }

func VerifyProcessIdentity(record Record) error { return nil }
