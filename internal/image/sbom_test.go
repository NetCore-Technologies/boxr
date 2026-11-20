package image

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSBOMHashesFilesAndRecordsSymlinksWithoutFollowing(t *testing.T) {
	useStore(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	content := []byte("boxr filesystem inventory\n")
	if err := os.WriteFile(filepath.Join(root, "etc", "boxr.conf"), content, 0640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(outside, []byte("must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc", "host-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFSWithConfig(root, "demo:sbom", "linux", "arm64", "v8", ImageRuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	report, err := GenerateSBOM("demo:sbom", 100)
	if err != nil {
		t.Fatalf("GenerateSBOM: %v", err)
	}
	if report.Kind != "boxr.filesystem-inventory" || report.Manifest == "" || report.Architecture != "arm64" || report.Variant != "v8" || report.FileCount != 1 || report.DirectoryCount != 1 || report.SymlinkCount != 1 {
		t.Fatalf("unexpected SBOM metadata: %+v", report)
	}
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])
	foundFile, foundLink := false, false
	for _, entry := range report.Entries {
		switch entry.Path {
		case "etc/boxr.conf":
			foundFile = entry.Type == "file" && entry.SHA256 == wantHash && entry.Size == int64(len(content)) && entry.Mode == "0640"
		case "etc/host-link":
			foundLink = entry.Type == "symlink" && entry.LinkTarget != "" && entry.SHA256 == ""
		}
	}
	if !foundFile || !foundLink {
		t.Fatalf("SBOM did not inventory expected file and symlink: %+v", report.Entries)
	}
}

func TestGenerateSBOMRejectsInvalidEntryLimitAndOverflow(t *testing.T) {
	useStore(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two"), []byte("2"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(root, "demo:sbom-limit"); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateSBOM("demo:sbom-limit", 0); err == nil {
		t.Fatal("zero entry limit unexpectedly accepted")
	}
	if _, err := GenerateSBOM("demo:sbom-limit", 1); err == nil {
		t.Fatal("entry limit overflow unexpectedly accepted")
	}
}

func TestGenerateSBOMRejectsMissingImage(t *testing.T) {
	useStore(t)
	if _, err := GenerateSBOM("demo:missing-sbom", 10); err == nil {
		t.Fatal("missing image unexpectedly accepted")
	}
}
