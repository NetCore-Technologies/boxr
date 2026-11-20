//go:build linux && !amd64 && !arm64

package container

import "fmt"

func applySeccompProfile(profile string) error {
	if profile == "" {
		return nil
	}
	return fmt.Errorf("seccomp profile %q is not implemented for this Linux architecture", profile)
}

func seccompProfileSupported(profile string) bool { return profile == "" }
