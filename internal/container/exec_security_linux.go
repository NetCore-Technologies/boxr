//go:build linux

package container

import "fmt"

// ApplyExecSecurity applies the per-process restrictions configured for the
// existing container to an additional workload process.
func ApplyExecSecurity(options Options) error {
	if len(options.DropCapabilities) > 0 {
		if err := dropLinuxCapabilities(options.DropCapabilities); err != nil {
			return fmt.Errorf("drop capabilities: %w", err)
		}
	}
	if options.NoNewPrivileges || len(options.DropCapabilities) > 0 || options.SeccompProfile != "" {
		if err := setNoNewPrivileges(); err != nil {
			return fmt.Errorf("set no-new-privileges: %w", err)
		}
	}
	if options.SeccompProfile != "" {
		if err := applySeccompProfile(options.SeccompProfile); err != nil {
			return fmt.Errorf("apply seccomp profile %q: %w", options.SeccompProfile, err)
		}
	}
	return nil
}
