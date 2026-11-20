package rootfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDigestIsStableAndContentSensitive(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "etc", "os-release"), []byte("NAME=Alpine\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("os-release", filepath.Join(root, "etc", "release")); err != nil {
			t.Fatal(err)
		}
	}
	a, err := Digest(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Digest(second)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("identical trees have different digests: %s != %s", a, b)
	}
	if err := os.WriteFile(filepath.Join(second, "etc", "os-release"), []byte("NAME=Debian\n"), 0644); err != nil {
		t.Fatal(err)
	}
	b, err = Digest(second)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("content change did not change digest")
	}
}

func TestDiffReportsAddedRemovedAndModified(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(before, "same"), []byte("same"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(before, "changed"), []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after, "same"), []byte("same"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after, "changed"), []byte("after"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after, "new"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(before, "gone"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	changes, err := Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("got %#v", changes)
	}
	if changes[0].Path != "changed" || changes[0].Status != "modified" {
		t.Fatalf("unexpected first diff: %#v", changes[0])
	}
	if changes[1].Path != "gone" || changes[1].Status != "removed" {
		t.Fatalf("unexpected second diff: %#v", changes[1])
	}
	if changes[2].Path != "new" || changes[2].Status != "added" {
		t.Fatalf("unexpected third diff: %#v", changes[2])
	}
}

func TestNormalizeDigest(t *testing.T) {
	valid := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := NormalizeDigest(valid)
	if err != nil || len(got) != 64 {
		t.Fatalf("NormalizeDigest = %q, %v", got, err)
	}
	for _, value := range []string{"", "sha256:xyz", "0123", "sha256:" + string(make([]byte, 64))} {
		if _, err := NormalizeDigest(value); err == nil {
			t.Errorf("accepted invalid digest %q", value)
		}
	}
}
