//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/creack/pty"
	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestExecRequiresContainerAndCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"exec"}, &out, &errOut); code != 2 {
		t.Fatalf("exec without arguments returned %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Usage: boxr exec") {
		t.Fatalf("exec usage missing: %q", errOut.String())
	}
}

func TestExecRejectsInvalidEnvironmentBeforeContainerLookup(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"exec", "--env", "BAD-NAME=value", "missing", "/bin/true"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "invalid --env") {
		t.Fatalf("invalid environment returned %d: %q", code, errOut.String())
	}
}

func TestExecRejectsRelativeWorkingDirectory(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"exec", "--workdir", "relative", "missing", "/bin/true"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "absolute path") {
		t.Fatalf("relative workdir returned %d: %q", code, errOut.String())
	}
}

func TestExecRejectsContainerThatIsNotRunning(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("exec-created", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"exec", record.ID, "/bin/true"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "not running") {
		t.Fatalf("exec for created container returned %d: %q", code, errOut.String())
	}
}

func TestExecRejectsContainerWithInvalidNamespacePID(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("exec-invalid-pid", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	originalVerifier := verifyContainerNamespacePID
	verifyContainerNamespacePID = func(pid int) error { return os.ErrPermission }
	t.Cleanup(func() { verifyContainerNamespacePID = originalVerifier })
	var out, errOut bytes.Buffer
	code := run([]string{"exec", record.ID, "/bin/true"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "namespace verification failed") {
		t.Fatalf("exec for invalid namespace PID returned %d: %q", code, errOut.String())
	}
}

func TestParseProcEnvironmentCollapsesDuplicateKeys(t *testing.T) {
	environment, err := parseProcEnvironment([]byte("PATH=/bin\x00TOKEN=first=value\x00PATH=/usr/bin\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(environment) != 2 || environment[0] != "PATH=/usr/bin" || environment[1] != "TOKEN=first=value" {
		t.Fatalf("parsed environment = %#v", environment)
	}
}

func TestExecNeedsContainerCgroupMatchesResourceControls(t *testing.T) {
	if execNeedsContainerCgroup(container.Options{}) {
		t.Fatal("empty options unexpectedly require a cgroup")
	}
	cases := []container.Options{
		{MemoryLimitBytes: 1}, {MemoryHighBytes: 1}, {MemoryLowBytes: 1},
		{MemoryMinBytes: 1}, {MemorySwapMaxBytes: 1}, {MemoryOOMGroup: true},
		{CPUs: 0.5}, {CPUWeight: 100}, {IOWeight: 100}, {PidsLimit: 10},
	}
	for index, options := range cases {
		if !execNeedsContainerCgroup(options) {
			t.Errorf("resource option case %d did not require cgroup", index)
		}
	}
}

func TestFindExecPathUsesContainerPath(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "example-command")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	path, err := findExecPath("example-command", []string{"PATH=" + dir})
	if err != nil {
		t.Fatal(err)
	}
	if path != binary {
		t.Fatalf("resolved path = %q, want %q", path, binary)
	}
	if _, err := findExecPath("missing-command", []string{"PATH=" + dir}); err == nil {
		t.Fatal("missing executable was resolved")
	}
}

func TestOpenProcDirectoryRejectsNonDirectory(t *testing.T) {
	if _, err := openProcDirectory(os.Getpid(), "stat"); err == nil {
		t.Fatal("opened /proc/PID/stat as a directory")
	}
}

func TestExecHelpDocumentsInteractiveAndPTYFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"exec", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("exec --help returned %d: %q", code, errOut.String())
	}
	for _, option := range []string{"-i", "-t", "--interactive", "--tty"} {
		if !strings.Contains(errOut.String(), option) {
			t.Errorf("exec help does not document %s: %q", option, errOut.String())
		}
	}
}

func TestFirstExecTerminalSelectsFirstAvailableTerminal(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("PTY device is unavailable: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if got := firstExecTerminal(nil, slave, master); got != slave {
		t.Fatalf("selected terminal = %v, want first terminal %v", got, slave)
	}
	if got := firstExecTerminal(nil); got != nil {
		t.Fatalf("selected terminal without candidates = %v, want nil", got)
	}
}

func TestExpandExecShortFlagsSupportsDockerStyleCombinedOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "interactive and tty",
			args: []string{"-it", "web", "/bin/sh"},
			want: []string{"-i", "-t", "web", "/bin/sh"},
		},
		{
			name: "reverse order after value option",
			args: []string{"--workdir", "/app", "-ti", "web", "/bin/sh"},
			want: []string{"--workdir", "/app", "-t", "-i", "web", "/bin/sh"},
		},
		{
			name: "do not rewrite command arguments",
			args: []string{"web", "/bin/sh", "-it"},
			want: []string{"web", "/bin/sh", "-it"},
		},
		{
			name: "do not rewrite arguments after separator",
			args: []string{"--", "-it", "web", "/bin/sh"},
			want: []string{"--", "-it", "web", "/bin/sh"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := expandExecShortFlags(test.args)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("expanded arguments = %#v, want %#v", got, test.want)
			}
		})
	}
}
