package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestAddManagedNetworkHostsMountAddsPrivateReadOnlyBind(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rootfs bind mounts are Linux-only")
	}
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	t.Setenv("BOXR_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	record, err := state.Create("hosts-mount", []string{"/bin/true"}, container.Options{
		RootFS: filepath.Join(t.TempDir(), "rootfs"), NetworkMode: "none",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	options := record.Options
	if err := addManagedNetworkHostsMount(record.ID, &options); err != nil {
		t.Fatal(err)
	}
	if len(options.BindMounts) != 1 {
		t.Fatalf("managed bind mounts = %+v, want one", options.BindMounts)
	}
	mount := options.BindMounts[0]
	if mount.Target != "/etc/hosts" || !mount.ReadOnly || mount.Source == "" {
		t.Fatalf("managed hosts mount = %+v", mount)
	}
	if _, err := os.Stat(mount.Source); err != nil {
		t.Fatalf("managed source file missing: %v", err)
	}
	persisted, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Options.BindMounts) != 1 || filepath.Clean(persisted.Options.BindMounts[0].Source) != filepath.Clean(mount.Source) {
		t.Fatalf("managed hosts mount was not persisted before startup: %+v", persisted.Options.BindMounts)
	}
}

func TestAddManagedNetworkHostsMountRespectsUserMounts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rootfs bind mounts are Linux-only")
	}
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	source := filepath.Join(t.TempDir(), "custom-hosts")
	if err := os.WriteFile(source, []byte("127.0.0.1 localhost\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/etc/hosts", "/etc"} {
		options := &container.Options{
			RootFS: filepath.Join(t.TempDir(), "rootfs"), NetworkMode: "none",
			BindMounts: []container.BindMount{{Source: source, Target: target}},
		}
		if err := addManagedNetworkHostsMount("hosts-user-"+strings.ReplaceAll(target, "/", "-"), options); err != nil {
			t.Fatalf("user bind target %s: %v", target, err)
		}
		if len(options.BindMounts) != 1 {
			t.Fatalf("user bind target %s was replaced: %+v", target, options.BindMounts)
		}
	}
	options := &container.Options{
		RootFS: filepath.Join(t.TempDir(), "rootfs"), NetworkMode: "none",
		BindMounts: []container.BindMount{{Source: source, Target: "/etc/hosts/custom"}},
	}
	if err := addManagedNetworkHostsMount("hosts-nested-bind", options); err == nil {
		t.Fatal("nested bind under /etc/hosts unexpectedly accepted")
	}
	tmpfsOptions := &container.Options{
		RootFS: filepath.Join(t.TempDir(), "rootfs"), NetworkMode: "none",
		TmpfsMounts: []container.TmpfsMount{{Target: "/etc"}},
	}
	if err := addManagedNetworkHostsMount("hosts-tmpfs", tmpfsOptions); err != nil {
		t.Fatal(err)
	}
	if len(tmpfsOptions.BindMounts) != 0 {
		t.Fatal("managed hosts mount shadowed a user /etc tmpfs")
	}
}

func TestAddManagedNetworkHostsMountSkipsHostAndUnrootedContainers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rootfs bind mounts are Linux-only")
	}
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	for _, options := range []*container.Options{
		{NetworkMode: "host"},
		{RootFS: filepath.Join(t.TempDir(), "rootfs"), NetworkMode: "host"},
		{NetworkMode: "none"},
	} {
		before := len(options.BindMounts)
		if err := addManagedNetworkHostsMount("hosts-skip", options); err != nil {
			t.Fatal(err)
		}
		if len(options.BindMounts) != before {
			t.Fatalf("unexpected managed mount for options %+v", options)
		}
	}
}

func TestManagedHostsMountPathConflictDetection(t *testing.T) {
	cases := []struct {
		target  string
		covered bool
		nested  bool
	}{
		{target: "/etc/hosts", covered: true},
		{target: "/etc", covered: true},
		{target: "/etc/hosts/child", nested: true},
		{target: "/var/run", covered: false, nested: false},
	}
	for _, test := range cases {
		covered, nested := managedHostsMountConflict(test.target)
		if covered != test.covered || nested != test.nested {
			t.Errorf("managedHostsMountConflict(%q) = %v,%v, want %v,%v", test.target, covered, nested, test.covered, test.nested)
		}
	}
}
