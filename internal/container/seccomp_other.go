//go:build !linux

package container

import "fmt"

func applySeccompProfile(profile string) error {
	if profile == "" {
		return nil
	}
	return fmt.Errorf("seccomp profiles are only available on Linux")
}

func seccompProfileSupported(profile string) bool { return profile == "" }
