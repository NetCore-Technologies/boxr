package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
)

func TestImageDUAllReportsTaggedLayersAndUniqueStoreBytes(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	importImage := func(reference, payload string) imageStore.Image {
		t.Helper()
		root := filepath.Join(t.TempDir(), "rootfs")
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "etc", "payload"), []byte(payload), 0644); err != nil {
			t.Fatal(err)
		}
		image, err := imageStore.ImportRootFS(root, reference)
		if err != nil {
			t.Fatalf("import %s: %v", reference, err)
		}
		return image
	}
	first := importImage("demo:a", "alpha payload")
	second := importImage("demo:b", "beta payload longer")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"image", "du", "--all", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("image du --all returned %d: %s", code, stderr.String())
	}
	var report imageUsageSummary
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode image usage summary: %v: %s", err, stdout.String())
	}
	wantReferenced := first.LayerBytes + second.LayerBytes
	if report.ImageCount != 2 || len(report.Images) != 2 || report.ReferencedLayerBytes != wantReferenced || report.Store.Images != 2 || report.Store.TotalBytes <= 0 {
		t.Fatalf("unexpected all-image usage report: %#v", report)
	}
	if report.Images[0].Reference != "demo:a" || report.Images[0].LayerBytes != first.LayerBytes ||
		report.Images[1].Reference != "demo:b" || report.Images[1].LayerBytes != second.LayerBytes {
		t.Fatalf("unexpected per-tag entries: %#v", report.Images)
	}
}

func TestImageDUAllFiltersTagsAndLegacySummaryRemainsCompatible(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "app"), []byte("app"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := imageStore.ImportRootFS(root, "sample:latest"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"image", "du", "--all", "--filter", "reference=sample:", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("filtered image du returned %d: %s", code, stderr.String())
	}
	var report imageUsageSummary
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ImageCount != 1 || len(report.Images) != 1 || report.Images[0].Reference != "sample:latest" {
		t.Fatalf("unexpected filtered image usage: %#v", report)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"image", "du", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("legacy image du --json returned %d: %s", code, stderr.String())
	}
	var legacy imageStore.StoreUsage
	if err := json.Unmarshal(stdout.Bytes(), &legacy); err != nil {
		t.Fatalf("decode legacy store usage: %v", err)
	}
	if legacy.Images != 1 || legacy.TotalBytes <= 0 {
		t.Fatalf("legacy store usage changed: %#v", legacy)
	}
}
