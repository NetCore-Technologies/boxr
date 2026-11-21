package engineapi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateDockerBindSourceRejectsSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := validateDockerBindSource(filepath.Join(link, "file")); err == nil {
		t.Fatal("bind source containing a symlink component was accepted")
	}
}

func TestValidateDockerBindSourceAcceptsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerBindSource(path); err != nil {
		t.Fatalf("valid regular-file bind source rejected: %v", err)
	}
}
