//go:build linux

package container

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverlayLockSerializesSharedUpperWorkDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "overlay")
	first, err := acquireOverlayLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireOverlayLock(dir); err == nil {
		t.Fatal("concurrent overlay lock was accepted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireOverlayLock(dir)
	if err != nil {
		t.Fatalf("overlay lock was not released: %v", err)
	}
	_ = second.Close()
}

func TestEnsureOverlayDirectoryRejectsSymlink(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(parent, "overlay")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ensureOverlayDirectory(link); err == nil {
		t.Fatal("symlinked overlay directory accepted")
	}
}

func TestPrepareOverlayRootRejectsNestedAndOptionDelimiterPaths(t *testing.T) {
	parent := t.TempDir()
	lower := filepath.Join(parent, "lower")
	if err := os.Mkdir(lower, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareOverlayRoot(lower, filepath.Join(lower, "overlay")); err == nil {
		t.Fatal("overlay storage nested inside lower rootfs was accepted")
	}
	if _, err := prepareOverlayRoot(lower, filepath.Join(parent, "overlay,invalid")); err == nil {
		t.Fatal("overlay mount option delimiter was accepted")
	}
}

func TestAcquireOverlayLockRejectsSymlinkLockFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-lock")
	if err := os.WriteFile(outside, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".boxr.lock")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := acquireOverlayLock(dir); err == nil {
		t.Fatal("symlinked lock file was accepted")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sentinel" {
		t.Fatalf("outside lock target was modified: %q", got)
	}
}

func TestAcquireOverlayLockCanonicalizesParentAliases(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0700); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(realParent, "overlay")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(parent, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	first, err := acquireOverlayLock(realDir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := acquireOverlayLock(filepath.Join(aliasParent, "overlay")); err == nil {
		t.Fatal("an alias to active OverlayFS storage acquired a second lock")
	}
}

func TestAcquireOverlayLockUsesPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireOverlayLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	info, err := os.Stat(filepath.Join(dir, ".boxr.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("lock mode = %04o, want 0600", got)
	}
}
