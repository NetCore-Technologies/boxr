//go:build !linux

package container

import "runtime"

// Doctor explains why namespace-based execution is unavailable off Linux.
func Doctor() []Diagnostic {
	return []Diagnostic{{
		Name:   "Platform",
		Status: DiagnosticError,
		Detail: "container execution requires a Linux kernel; current platform is " + runtime.GOOS + "/" + runtime.GOARCH,
	}}
}
