//go:build linux && (amd64 || arm64)

package container

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	bpfLDWABS          = 0x20
	bpfJMPJEQ          = 0x15
	bpfRETK            = 0x06
	seccompReturnKill  = 0x80000000
	seccompReturnErrno = 0x00050000
	seccompReturnAllow = 0x7fff0000
	prSetNoNewPrivs    = 38
	prSetSeccomp       = 22
	seccompModeFilter  = 2
)

func applySeccompProfile(profile string) error {
	program, err := buildSeccompFilter(profile)
	if err != nil {
		return err
	}
	if err := setNoNewPrivileges(); err != nil {
		return err
	}
	filter := syscall.SockFprog{Len: uint16(len(program)), Filter: &program[0]}
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&filter)))
	runtime.KeepAlive(program)
	runtime.KeepAlive(&filter)
	if errno != 0 {
		return errno
	}
	return nil
}

func buildSeccompFilter(profile string) ([]syscall.SockFilter, error) {
	if profile != "baseline" {
		return nil, fmt.Errorf("unsupported profile %q", profile)
	}
	arch, denied := seccompArchitectureAndDenylist()
	program := make([]syscall.SockFilter, 0, 4+len(denied)*2)
	// Refuse a syscall ABI mismatch rather than applying an architecture-specific
	// syscall list to a different ABI.
	program = append(program,
		syscall.SockFilter{Code: bpfLDWABS, K: 4},
		syscall.SockFilter{Code: bpfJMPJEQ, Jt: 1, Jf: 0, K: arch},
		syscall.SockFilter{Code: bpfRETK, K: seccompReturnKill},
		syscall.SockFilter{Code: bpfLDWABS, K: 0},
	)
	for _, number := range denied {
		program = append(program,
			syscall.SockFilter{Code: bpfJMPJEQ, Jt: 0, Jf: 1, K: number},
			syscall.SockFilter{Code: bpfRETK, K: seccompReturnErrno | uint32(syscall.EPERM)},
		)
	}
	program = append(program, syscall.SockFilter{Code: bpfRETK, K: seccompReturnAllow})
	return program, nil
}

func seccompProfileSupported(profile string) bool { return profile == "" || profile == "baseline" }
