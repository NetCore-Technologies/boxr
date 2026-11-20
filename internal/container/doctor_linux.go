//go:build linux

package container

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const capSysAdmin = 21

// Doctor inspects the current Linux host without changing system state.
func Doctor() []Diagnostic {
	checks := make([]Diagnostic, 0, 8)
	checks = append(checks, Diagnostic{
		Name:   "Platform",
		Status: DiagnosticOK,
		Detail: "Linux kernel detected; namespace-based execution is available on this platform",
	})

	for _, namespace := range []string{"pid", "uts", "mnt", "ipc"} {
		path := "/proc/self/ns/" + namespace
		if _, err := os.Stat(path); err != nil {
			checks = append(checks, Diagnostic{
				Name:   namespace + " namespace",
				Status: DiagnosticError,
				Detail: fmt.Sprintf("cannot inspect %s: %v", path, err),
			})
			continue
		}
		checks = append(checks, Diagnostic{
			Name:   namespace + " namespace",
			Status: DiagnosticOK,
			Detail: "kernel namespace handle is available",
		})
	}

	hasAdmin, err := effectiveCapability(capSysAdmin)
	switch {
	case err != nil:
		checks = append(checks, Diagnostic{Name: "Namespace privilege", Status: DiagnosticWarn, Detail: "could not inspect effective Linux capabilities: " + err.Error()})
	case hasAdmin:
		checks = append(checks, Diagnostic{Name: "Namespace privilege", Status: DiagnosticOK, Detail: "CAP_SYS_ADMIN is present in the effective capability set"})
	default:
		checks = append(checks, Diagnostic{Name: "Namespace privilege", Status: DiagnosticWarn, Detail: "CAP_SYS_ADMIN is not effective; boxr run will normally be denied unless the host grants the required namespace capability"})
	}

	checks = append(checks, cgroupDiagnostics()...)
	checks = append(checks, Diagnostic{
		Name:   "Security boundary",
		Status: DiagnosticWarn,
		Detail: "filesystem and network are shared with the host; do not run untrusted workloads",
	})
	return checks
}

func effectiveCapability(number uint) (bool, error) {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return false, err
	}
	defer file.Close()
	return effectiveCapabilityFrom(file, number)
}

func effectiveCapabilityFrom(input io.Reader, number uint) (bool, error) {
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "CapEff:" {
			continue
		}
		var mask uint64
		if _, err := fmt.Sscanf(fields[1], "%x", &mask); err != nil {
			return false, fmt.Errorf("parse CapEff: %w", err)
		}
		if number >= 64 {
			return false, fmt.Errorf("capability number %d is out of range", number)
		}
		return mask&(uint64(1)<<number) != 0, nil
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("CapEff field not found in /proc/self/status")
}

func cgroupDiagnostics() []Diagnostic {
	return cgroupDiagnosticsAt("/sys/fs/cgroup")
}

func cgroupDiagnosticsAt(root string) []Diagnostic {
	controllersBytes, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return []Diagnostic{{Name: "cgroup v2", Status: DiagnosticWarn, Detail: "unified cgroup v2 is not available at /sys/fs/cgroup; --memory and --cpus cannot be used"}}
	}
	available := make(map[string]bool)
	for _, controller := range strings.Fields(string(controllersBytes)) {
		available[controller] = true
	}

	enabledBytes, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return []Diagnostic{{Name: "cgroup v2", Status: DiagnosticWarn, Detail: "cannot read the root cgroup subtree controller delegation: " + err.Error()}}
	}
	enabled := make(map[string]bool)
	for _, controller := range strings.Fields(string(enabledBytes)) {
		enabled[strings.TrimPrefix(controller, "+")] = true
	}

	missing := make([]string, 0, 2)
	for _, controller := range []string{"cpu", "memory"} {
		switch {
		case !available[controller]:
			missing = append(missing, controller+" (not available)")
		case !enabled[controller]:
			missing = append(missing, controller+" (not delegated to child cgroups)")
		}
	}
	if len(missing) > 0 {
		return []Diagnostic{{Name: "cgroup v2", Status: DiagnosticWarn, Detail: "resource limits may be unavailable: " + strings.Join(missing, ", ")}}
	}
	return []Diagnostic{{Name: "cgroup v2", Status: DiagnosticOK, Detail: "CPU and memory controllers are available and delegated at the cgroup root"}}
}
