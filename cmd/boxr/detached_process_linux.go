//go:build linux

package main

import "syscall"

func detachedProcessSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
