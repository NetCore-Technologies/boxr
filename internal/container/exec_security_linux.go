//go:build linux

package container

import "fmt"

// ApplyExecSecurity applies the per-process restrictions configured for the
// existing container to an additional workload process.
func ApplyExecSecurity(options Options) error {
	return ApplyExecSecurityWithUser(options, false)
}

// ApplyExecSecurityWithUser applies the container security policy while
// optionally preserving CAP_SETUID until a non-root exec user is selected.
func ApplyExecSecurityWithUser(options Options, preserveSetUID bool) error {
	if err := DropExecCapabilities(options, preserveSetUID); err != nil {
		return err
	}
	return ApplyExecRestrictions(options)
}

// DropExecCapabilities removes configured capabilities before switching the
// workload to its configured UID.
func DropExecCapabilities(options Options, preserveSetUID bool) error {
	if len(options.DropCapabilities) == 0 {
		return nil
	}
	var err error
	if preserveSetUID {
		err = dropLinuxCapabilitiesPreservingSetUID(options.DropCapabilities)
	} else {
		err = dropLinuxCapabilities(options.DropCapabilities)
	}
	if err != nil {
		return fmt.Errorf("drop capabilities: %w", err)
	}
	return nil
}

// ApplyExecRestrictions installs no-new-privileges and seccomp after any UID
// transition, so custom filters cannot accidentally block the setuid syscall.
func ApplyExecRestrictions(options Options) error {
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


// PrepareExecUser resolves an exec USER against the container root filesystem.
// The returned closure performs the final UID switch after capabilities drop.
func PrepareExecUser(spec string, userNamespace bool) (func() error, bool, error) {
	return prepareContainerUser(spec, userNamespace)
}
