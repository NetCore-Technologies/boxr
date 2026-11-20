package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
)

func TestImagePruneJSONReportsReclaimableBlobSizes(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "images")
	t.Setenv("BOXR_IMAGE_DIR", storeDir)
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "app"), []byte("app"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := imageStore.ImportRootFS(root, "demo:latest"); err != nil {
		t.Fatal(err)
	}
	payload := []byte("orphan blob for CLI prune report")
	digest := strings.Repeat("a", 64)
	blobPath := filepath.Join(storeDir, "blobs", "sha256", digest)
	if err := os.WriteFile(blobPath, payload, 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"image", "prune", "--dry-run", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("image prune dry-run returned %d: %s", code, stderr.String())
	}
	var report imagePruneReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode image prune report: %v: %s", err, stdout.String())
	}
	if !report.DryRun || report.BlobCount != 1 || report.ReclaimableBytes != int64(len(payload)) ||
		len(report.Blobs) != 1 || report.Blobs[0].Digest != "sha256:"+digest || report.Blobs[0].SizeBytes != int64(len(payload)) {
		t.Fatalf("unexpected dry-run report: %#v", report)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("dry-run removed blob: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"image", "prune", "--force", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("image prune --force returned %d: %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.DryRun || report.BlobCount != 1 || report.ReclaimableBytes != int64(len(payload)) {
		t.Fatalf("unexpected destructive report: %#v", report)
	}
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Fatalf("force prune did not remove blob: %v", err)
	}
}
