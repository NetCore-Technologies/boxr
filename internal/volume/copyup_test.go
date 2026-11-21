package volume

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyUpFromRootFSCopiesImageTreeOnlyIntoEmptyVolumes(t *testing.T) {
	useTempData(t)
	root := t.TempDir()
	source := filepath.Join(root, "var", "lib", "app")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(source, "nested", "state.db")
	if err := os.WriteFile(filePath, []byte("image-seeded-data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/state.db", filepath.Join(source, "current")); err != nil {
		t.Fatal(err)
	}
	item, err := Create("copy-up", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyUpFromRootFS(item.Name, root, "/var/lib/app"); err != nil {
		t.Fatalf("copy image path into empty volume: %v", err)
	}
	copied, err := os.ReadFile(filepath.Join(item.Path, "nested", "state.db"))
	if err != nil || string(copied) != "image-seeded-data" {
		t.Fatalf("copied image data = %q, err=%v", copied, err)
	}
	info, err := os.Stat(filepath.Join(item.Path, "nested", "state.db"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("copied file mode = %v, err=%v", info, err)
	}
	link, err := os.Readlink(filepath.Join(item.Path, "current"))
	if err != nil || link != "nested/state.db" {
		t.Fatalf("copied symlink = %q, err=%v", link, err)
	}
	if err := os.WriteFile(filePath, []byte("changed-image-data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := CopyUpFromRootFS(item.Name, root, "/var/lib/app"); err != nil {
		t.Fatalf("repeat copy-up: %v", err)
	}
	copied, err = os.ReadFile(filepath.Join(item.Path, "nested", "state.db"))
	if err != nil || string(copied) != "image-seeded-data" {
		t.Fatalf("repeat copy-up overwrote persistent data: %q, err=%v", copied, err)
	}
}

func TestCopyUpFromRootFSLeavesNonEmptyVolumesAndMissingTargetsAlone(t *testing.T) {
	useTempData(t)
	root := t.TempDir()
	source := filepath.Join(root, "data")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "image.txt"), []byte("image"), 0644); err != nil {
		t.Fatal(err)
	}
	item, err := Create("existing-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Path, "user.txt"), []byte("user"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CopyUpFromRootFS(item.Name, root, "/data"); err != nil {
		t.Fatalf("copy-up into non-empty volume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(item.Path, "image.txt")); !os.IsNotExist(err) {
		t.Fatalf("existing volume received image data: %v", err)
	}
	userData, err := os.ReadFile(filepath.Join(item.Path, "user.txt"))
	if err != nil || string(userData) != "user" {
		t.Fatalf("existing volume data changed: %q, err=%v", userData, err)
	}
	empty, err := Create("missing-target", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyUpFromRootFS(empty.Name, root, "/not-present"); err != nil {
		t.Fatalf("missing image mountpoint should be a no-op: %v", err)
	}
	entries, err := os.ReadDir(empty.Path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing image mountpoint changed volume: entries=%v err=%v", entries, err)
	}
}

func TestCopyUpFromRootFSRejectsSymlinkedMountTargetsAndEscapingLinks(t *testing.T) {
	useTempData(t)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	item, err := Create("unsafe-target", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyUpFromRootFS(item.Name, root, "/linked"); err == nil {
		t.Fatal("symlinked image mount target unexpectedly accepted")
	}
	entries, err := os.ReadDir(item.Path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink rejection changed volume: entries=%v err=%v", entries, err)
	}
	if err := os.Remove(filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "app")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := CopyUpFromRootFS(item.Name, root, "/app"); err == nil {
		t.Fatal("escaping image symlink unexpectedly copied into volume")
	}
	entries, err = os.ReadDir(item.Path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("escaping symlink rejection left partial volume data: entries=%v err=%v", entries, err)
	}
}
