package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndVerifyImageLock(t *testing.T) {
	useStore(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app"), []byte("release one"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFSWithConfig(root, "demo:locked", "linux", "arm64", "v8", ImageRuntimeConfig{Labels: map[string]string{"org.example.release": "one"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "boxr.lock.json")
	lock, err := WriteLock(path, "demo:locked")
	if err != nil {
		t.Fatalf("WriteLock: %v", err)
	}
	if lock.SchemaVersion != 1 || lock.OS != "linux" || lock.Architecture != "arm64" || lock.Variant != "v8" || !strings.HasPrefix(lock.Digest, "sha256:") {
		t.Fatalf("unexpected lock: %+v", lock)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("lock permissions = %o, want 600", info.Mode().Perm())
	}
	verified, err := VerifyLock(path)
	if err != nil {
		t.Fatalf("VerifyLock: %v", err)
	}
	if verified.Digest != lock.Digest {
		t.Fatalf("verified digest = %s, want %s", verified.Digest, lock.Digest)
	}
}

func TestImageLockDetectsMovedTag(t *testing.T) {
	useStore(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "version"), []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(root, "demo:mutable"); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "boxr.lock.json")
	if _, err := WriteLock(lockPath, "demo:mutable"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "version"), []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(root, "demo:mutable"); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLock(lockPath); err == nil || !strings.Contains(err.Error(), "image lock mismatch") {
		t.Fatalf("moved tag verification error = %v, want mismatch", err)
	}
}

func TestReadLockRejectsUnsafeAndUnknownContent(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	valid := `{"schemaVersion":1,"reference":"demo:tag","digest":"sha256:` + strings.Repeat("a", 64) + `","os":"linux","architecture":"amd64"}`
	if err := os.WriteFile(outside, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLock(link); err == nil {
		t.Fatal("symlinked lock unexpectedly accepted")
	}
	for name, content := range map[string]string{
		"unknown.json":    valid[:len(valid)-1] + `,"extra":true}`,
		"trailing.json":   valid + `{}`,
		"bad-digest.json": `{"schemaVersion":1,"reference":"demo:tag","digest":"sha256:bad","os":"linux","architecture":"amd64"}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLock(path); err == nil {
			t.Errorf("unsafe lock %s unexpectedly accepted", name)
		}
	}
}

func TestWriteLockRejectsSymlinkDestinationAndParent(t *testing.T) {
	useStore(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(root, "demo:lock-safe"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do not replace"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteLock(link, "demo:lock-safe"); err == nil {
		t.Fatal("symlink destination unexpectedly accepted")
	}
	parentLink := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(filepath.Dir(outside), parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteLock(filepath.Join(parentLink, "boxr.lock.json"), "demo:lock-safe"); err == nil {
		t.Fatal("symlinked parent unexpectedly accepted")
	}
}
