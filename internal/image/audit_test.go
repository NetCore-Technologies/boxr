package image

import (
	"strings"
	"testing"
)

func TestAuditReportsVerifiedImageGraphAndRootFS(t *testing.T) {
	useStore(t)
	root := newRoot(t)
	if _, err := ImportRootFSForPlatform(root, "demo:audit", "linux", "arm64", "v8"); err != nil {
		t.Fatal(err)
	}

	report, err := Audit("demo:audit", 100)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if !report.IntegrityVerified {
		t.Fatal("image graph was not marked verified")
	}
	if report.Image.OS != "linux" || report.Image.Arch != "arm64" || report.Image.Variant != "v8" {
		t.Fatalf("unexpected platform: %+v", report.Image)
	}
	if !strings.HasPrefix(report.ManifestDigest, "sha256:") || !strings.HasPrefix(report.ConfigDigest, "sha256:") {
		t.Fatalf("missing descriptor digests: %+v", report)
	}
	if report.LayerCount != 1 || len(report.LayerDigests) != 1 || len(report.LayerMediaTypes) != 1 {
		t.Fatalf("unexpected layer summary: %+v", report)
	}
	if report.RootFS.EntriesScanned == 0 || report.RootFS.Root != "demo:audit" {
		t.Fatalf("unexpected rootfs audit: %+v", report.RootFS)
	}
}

func TestAuditRejectsMissingImageAndInvalidLimit(t *testing.T) {
	useStore(t)
	if _, err := Audit("missing:latest", 10); err == nil {
		t.Fatal("Audit unexpectedly accepted a missing image")
	}
	if _, err := Audit("missing:latest", 0); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("unexpected invalid-limit error: %v", err)
	}
}
