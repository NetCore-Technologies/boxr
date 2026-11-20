//go:build linux

package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectiveCapabilityFromReadsEffectiveMask(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		cap  uint
		want bool
	}{
		{name: "present", data: "Name:\tboxr\nCapEff:\t0000000000200000\n", cap: capSysAdmin, want: true},
		{name: "absent", data: "CapEff:\t0000000000000000\n", cap: capSysAdmin, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := effectiveCapabilityFrom(strings.NewReader(test.data), test.cap)
			if err != nil {
				t.Fatalf("effectiveCapabilityFrom(): %v", err)
			}
			if got != test.want {
				t.Fatalf("effectiveCapabilityFrom() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestEffectiveCapabilityFromRejectsInvalidStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		cap  uint
	}{
		{name: "missing field", data: "Name:\tboxr\n", cap: capSysAdmin},
		{name: "invalid mask", data: "CapEff:\txyz\n", cap: capSysAdmin},
		{name: "out of range", data: "CapEff:\t0000000000000000\n", cap: 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := effectiveCapabilityFrom(strings.NewReader(test.data), test.cap); err == nil {
				t.Fatal("expected malformed capability status to fail")
			}
		})
	}
}

func TestCgroupDiagnosticsAtReportsControllerState(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("cgroup.controllers", "cpu memory pids")
	write("cgroup.subtree_control", "+cpu +memory")
	checks := cgroupDiagnosticsAt(root)
	if len(checks) != 1 || checks[0].Status != DiagnosticOK {
		t.Fatalf("fully delegated controllers = %#v, want one OK diagnostic", checks)
	}

	write("cgroup.subtree_control", "+cpu")
	checks = cgroupDiagnosticsAt(root)
	if len(checks) != 1 || checks[0].Status != DiagnosticWarn || !strings.Contains(checks[0].Detail, "memory") {
		t.Fatalf("missing memory delegation = %#v, want warning", checks)
	}

	write("cgroup.controllers", "pids")
	checks = cgroupDiagnosticsAt(root)
	if len(checks) != 1 || checks[0].Status != DiagnosticWarn || !strings.Contains(checks[0].Detail, "cpu (not available)") {
		t.Fatalf("missing controllers = %#v, want warning", checks)
	}
}

func TestCgroupDiagnosticsAtReportsUnavailableFilesystem(t *testing.T) {
	checks := cgroupDiagnosticsAt(filepath.Join(t.TempDir(), "missing"))
	if len(checks) != 1 || checks[0].Status != DiagnosticWarn || !strings.Contains(checks[0].Detail, "not available") {
		t.Fatalf("missing cgroup files = %#v, want warning", checks)
	}
}

func TestDoctorIncludesPlatformAndSecurityChecks(t *testing.T) {
	checks := Doctor()
	if len(checks) < 7 {
		t.Fatalf("Doctor() returned only %d checks", len(checks))
	}
	if checks[0].Name != "Platform" || checks[0].Status != DiagnosticOK {
		t.Fatalf("platform check = %#v, want Linux OK", checks[0])
	}
	last := checks[len(checks)-1]
	if last.Name != "Security boundary" || last.Status != DiagnosticWarn {
		t.Fatalf("last check = %#v, want security warning", last)
	}
}
