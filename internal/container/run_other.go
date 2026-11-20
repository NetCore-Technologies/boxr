//go:build !linux

package container

import (
	"fmt"
	"runtime"
)

// Run is present on non-Linux platforms so the CLI and release binaries build
// consistently. Namespace-based container execution is Linux-kernel specific.
func Run(command string, args []string, options Options) error {
	return RunWithHooks(command, args, options, Hooks{})
}

// RunWithHooks preserves a consistent API on non-Linux systems.
func RunWithHooks(command string, args []string, options Options, hooks Hooks) error {
	return fmt.Errorf("container isolation requires Linux; current platform is %s/%s", runtime.GOOS, runtime.GOARCH)
}

// Init is unreachable on unsupported platforms; it exists to keep the shared
// command entrypoint buildable for cross-platform help and version binaries.
func Init(args []string) error {
	return fmt.Errorf("container initialization requires Linux")
}
