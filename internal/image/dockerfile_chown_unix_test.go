//go:build linux || darwin

package image

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestBuildDockerfileCopyChownAppliesNumericOwnerWithoutFollowingSymlinks(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	source := filepath.Join(contextDir, "payload")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "file.txt"), []byte("payload\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/file.txt", filepath.Join(source, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "single.txt"), []byte("single\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	uid, gid := os.Getuid(), os.Getgid()
	content := "FROM scratch\nCOPY --chown=" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid) + " payload /opt/payload\nCOPY --chown=" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid) + " single.txt /opt/single.txt\n"
	if err := os.WriteFile(dockerfile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:copy-chown", ""); err != nil {
		t.Fatalf("BuildDockerfileForPlatform: %v", err)
	}
	root := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:copy-chown", root); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		filepath.Join(root, "opt", "payload", "nested"),
		filepath.Join(root, "opt", "payload", "nested", "file.txt"),
		filepath.Join(root, "opt", "payload", "file-link"),
		filepath.Join(root, "opt", "single.txt"),
	} {
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("stat for %s has type %T", target, info.Sys())
		}
		if int(stat.Uid) != uid || int(stat.Gid) != gid {
			t.Errorf("owner for %s = %d:%d, want %d:%d", target, stat.Uid, stat.Gid, uid, gid)
		}
	}
}

func TestBuildDockerfileCopyChownAppliesToCopyFromStage(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(contextDir, "payload"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload", "file.txt"), []byte("stage payload\n"), 0644); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	content := "FROM scratch AS build\nCOPY payload /staged\nFROM scratch AS final\nCOPY --from=build --chown=" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid) + " /staged/file.txt /app/file.txt\n"
	if err := os.WriteFile(dockerfile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:copy-chown-stage", ""); err != nil {
		t.Fatalf("BuildDockerfileForPlatform: %v", err)
	}
	root := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:copy-chown-stage", root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "app", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Fatalf("COPY --from owner = %d:%d, want %d:%d", stat.Uid, stat.Gid, uid, gid)
	}
}

