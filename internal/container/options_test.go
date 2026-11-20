package container

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMergeEnvironmentAppliesOverridesWithoutDuplicateKeys(t *testing.T) {
	base := []string{"PATH=/usr/bin", "APP_ENV=production", "EMPTY=", "APP_ENV=staging"}
	overrides := []string{"APP_ENV=development", "NEW_SETTING=value"}
	got := mergeEnvironment(base, overrides)
	want := []string{"PATH=/usr/bin", "APP_ENV=development", "EMPTY=", "NEW_SETTING=value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvironment() = %#v, want %#v", got, want)
	}
}

func TestMergeEnvironmentIgnoresMalformedBaseEntries(t *testing.T) {
	got := mergeEnvironment([]string{"=invalid", "NO_EQUALS", "GOOD=value"}, nil)
	want := []string{"GOOD=value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvironment() = %#v, want %#v", got, want)
	}
}

func TestMergeEnvironmentDoesNotMutateInputs(t *testing.T) {
	base := []string{"A=one"}
	overrides := []string{"A=two"}
	_ = mergeEnvironment(base, overrides)
	if !reflect.DeepEqual(base, []string{"A=one"}) || !reflect.DeepEqual(overrides, []string{"A=two"}) {
		t.Fatalf("mergeEnvironment() mutated inputs: base=%#v overrides=%#v", base, overrides)
	}
}

func TestOptionsValidateRootFSRequirements(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		valid   bool
	}{
		{name: "default host mode", options: Options{NetworkMode: "host"}, valid: true},
		{name: "private network", options: Options{NetworkMode: "none"}, valid: true},
		{name: "unsupported network", options: Options{NetworkMode: "bridge"}},
		{name: "rootfs relative", options: Options{RootFS: "./rootfs"}},
		{name: "host root rejected", options: Options{RootFS: "/"}},
		{name: "read only requires rootfs", options: Options{ReadOnlyRootFS: true}},
		{name: "proc requires rootfs", options: Options{MountProc: true}},
		{name: "absolute workdir required in rootfs", options: Options{RootFS: "/tmp/rootfs", Workdir: "tmp"}},
		{name: "valid rootfs", options: Options{RootFS: "/tmp/rootfs", Workdir: "/tmp", MountProc: true}, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.options.Validate()
			if test.valid && err != nil {
				t.Fatalf("Validate(): %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestOptionsValidateResourceWeightsAndMountTargets(t *testing.T) {
	valid := Options{CPUWeight: 10000, IOWeight: 1, PidsLimit: 1, RootFS: "/tmp/rootfs", BindMounts: []BindMount{{Source: "/etc/hosts", Target: "/etc/hosts", ReadOnly: true}}, TmpfsMounts: []TmpfsMount{{Target: "/tmp", SizeBytes: 64 << 20, Mode: 01777}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	invalid := []Options{
		{CPUWeight: 10001}, {CPUWeight: -1}, {IOWeight: 10001}, {PidsLimit: -1},
		{RootFS: "/tmp/rootfs", BindMounts: []BindMount{{Source: "relative", Target: "/data"}}},
		{RootFS: "/tmp/rootfs", BindMounts: []BindMount{{Source: "/data", Target: "/../etc"}}},
		{RootFS: "/tmp/rootfs", TmpfsMounts: []TmpfsMount{{Target: "relative"}}},
		{DropCapabilities: []string{"DOES_NOT_EXIST"}},
	}
	for i, options := range invalid {
		if err := options.Validate(); err == nil {
			t.Errorf("invalid options %d unexpectedly succeeded", i)
		}
	}
}

func TestCapabilityNumberAndNormalization(t *testing.T) {
	for input, want := range map[string]int{"CHOWN": 0, "CAP_SYS_ADMIN": 21, "net_raw": 13, "checkpoint_restore": 40} {
		got, ok := capabilityNumber(input)
		if !ok || got != want {
			t.Errorf("capabilityNumber(%q) = %d, %t; want %d, true", input, got, ok, want)
		}
	}
	if _, ok := capabilityNumber("not-a-capability"); ok {
		t.Fatal("unknown capability was accepted")
	}
	got, all := normalizedCapabilities([]string{"CAP_NET_RAW", "CAP_NET_ADMIN", "NET_RAW"})
	if all || len(got) != 2 || got[0] != 12 || got[1] != 13 {
		t.Fatalf("normalized capabilities = %v, all=%t", got, all)
	}
	got, all = normalizedCapabilities([]string{"ALL"})
	if !all || len(got) != 41 || got[0] != 0 || got[len(got)-1] != 40 {
		t.Fatalf("ALL capabilities = %v, all=%t", got, all)
	}
}

func TestPrepareRootPathCreatesContainedDirectoryAndFile(t *testing.T) {
	root := t.TempDir()
	path, err := PrepareRootPath(root, "/etc/hosts", false)
	if err != nil {
		t.Fatalf("PrepareRootPath(): %v", err)
	}
	if path != filepath.Join(root, "etc", "hosts") {
		t.Fatalf("path = %q", path)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		t.Fatalf("expected file mountpoint, info=%v err=%v", info, err)
	}
	directory, err := PrepareRootPath(root, "/var/tmp", true)
	if err != nil {
		t.Fatalf("PrepareRootPath directory: %v", err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("expected directory mountpoint, info=%v err=%v", info, err)
	}
}

func TestPrepareRootPathRejectsSymlinkEscapeAndTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	for _, target := range []string{"../outside", "/escape/child"} {
		if _, err := PrepareRootPath(root, target, true); err == nil {
			t.Errorf("PrepareRootPath(%q) unexpectedly succeeded", target)
		}
	}
}

func TestParseTmpfsSize(t *testing.T) {
	for input, want := range map[string]int64{"64m": 64 << 20, "1GiB": 1 << 30, "4096": 4096, "2k": 2048} {
		got, err := ParseTmpfsSize(input)
		if err != nil || got != want {
			t.Errorf("ParseTmpfsSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "-1", "1.5g", "999999999999999999999g"} {
		if _, err := ParseTmpfsSize(input); input == "" {
			if err != nil {
				t.Errorf("empty size should mean no explicit cap: %v", err)
			}
		} else if err == nil {
			t.Errorf("ParseTmpfsSize(%q) unexpectedly succeeded", input)
		}
	}
}

func TestOverlayOptionsRequireSeparateSafeDirectories(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "boxr-rootfs")
	valid := filepath.Join(string(filepath.Separator), "tmp", "boxr-overlay")
	cases := []struct {
		name    string
		options Options
		wantErr string
	}{
		{name: "valid", options: Options{RootFS: root, OverlayDir: valid}},
		{name: "requires rootfs", options: Options{OverlayDir: valid}, wantErr: "--rootfs is required"},
		{name: "inside lower", options: Options{RootFS: root, OverlayDir: filepath.Join(root, "overlay")}, wantErr: "separate directory trees"},
		{name: "ancestor of lower", options: Options{RootFS: root, OverlayDir: "/tmp"}, wantErr: "separate directory trees"},
		{name: "root overlay", options: Options{RootFS: root, OverlayDir: "/"}, wantErr: "absolute directory"},
		{name: "comma option delimiter", options: Options{RootFS: "/tmp/root,one", OverlayDir: valid}, wantErr: "cannot contain commas"},
		{name: "colon option delimiter", options: Options{RootFS: root, OverlayDir: "/tmp/overlay:two"}, wantErr: "cannot contain commas"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.options.Validate()
			if test.wantErr == "" && err != nil {
				t.Fatalf("Validate(): %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestSeccompProfileValidation(t *testing.T) {
	if err := (Options{SeccompProfile: "baseline"}).Validate(); err != nil {
		t.Fatalf("baseline profile rejected: %v", err)
	}
	if err := (Options{SeccompProfile: "unrestricted"}).Validate(); err == nil {
		t.Fatal("unknown profile unexpectedly accepted")
	}
}
