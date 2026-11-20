//go:build linux && (amd64 || arm64)

package container

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestBuildBaselineSeccompFilter(t *testing.T) {
	program, err := buildSeccompFilter("baseline")
	if err != nil {
		t.Fatal(err)
	}
	if len(program) < 8 || len(program) > 4096 {
		t.Fatalf("unexpected BPF instruction count %d", len(program))
	}
	if program[0].Code != bpfLDWABS || program[0].K != 4 {
		t.Fatalf("missing architecture load: %+v", program[0])
	}
	if program[len(program)-1].Code != bpfRETK || program[len(program)-1].K != seccompReturnAllow {
		t.Fatalf("missing final allow instruction: %+v", program[len(program)-1])
	}
	foundErrno := false
	for _, instruction := range program {
		if instruction.Code == bpfRETK && instruction.K == seccompReturnErrno|uint32(syscall.EPERM) {
			foundErrno = true
			break
		}
	}
	if !foundErrno {
		t.Fatal("filter has no EPERM deny action")
	}
	if _, err := buildSeccompFilter("made-up"); err == nil {
		t.Fatal("unknown profile unexpectedly accepted")
	}
}

func TestSeccompBaselineSubprocess(t *testing.T) {
	if os.Getenv("BOXR_TEST_SECCOMP_CHILD") == "1" {
		if err := applySeccompProfile("baseline"); err != nil {
			t.Fatalf("apply seccomp: %v", err)
		}
		_, _, ptraceErr := syscall.RawSyscall(syscall.SYS_PTRACE, 0, 0, 0)
		if ptraceErr != syscall.EPERM {
			t.Fatalf("ptrace syscall error = %v, want EPERM", ptraceErr)
		}
		if err := syscall.Exec("/bin/true", []string{"true"}, []string{"PATH=/usr/bin:/bin"}); err != nil {
			t.Fatalf("exec after seccomp: %v", err)
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestSeccompBaselineSubprocess$")
	command.Env = append(os.Environ(), "BOXR_TEST_SECCOMP_CHILD=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("seccomp child failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "apply seccomp") {
		t.Fatalf("seccomp child output: %s", output)
	}
}
