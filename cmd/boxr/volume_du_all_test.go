package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/manit6752025/boxr/internal/volume"
)

func TestVolumeDUAllAggregatesUsageAndHonorsFilters(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	alpha, err := volume.Create("du-alpha", map[string]string{"team": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := volume.Create("du-beta", map[string]string{"team": "other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alpha.Path, "alpha.txt"), []byte("alpha"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(alpha.Path, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta.Path, "beta.txt"), []byte("beta-data"), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"volume", "du", "--all", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("volume du --all returned %d: %s", code, stderr.String())
	}
	var report volumeUsageSummary
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode all-volume usage: %v: %s", err, stdout.String())
	}
	if report.VolumeCount != 2 || len(report.Volumes) != 2 || report.Files != 2 || report.Directories != 1 || report.TotalBytes != int64(len("alpha")+len("beta-data")) {
		t.Fatalf("unexpected all-volume summary: %#v", report)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"volume", "du", "--all", "--filter", "label=team=demo", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("filtered volume du returned %d: %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.VolumeCount != 1 || len(report.Volumes) != 1 || report.Volumes[0].Name != alpha.Name || report.TotalBytes != int64(len("alpha")) {
		t.Fatalf("volume du filter did not select alpha: %#v", report)
	}
}

func TestVolumeDUSingleVolumeOutputRemainsCompatible(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	created, err := volume.Create("du-single", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "payload"), []byte("single"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"volume", "du", "--json", created.Name}, &stdout, &stderr); code != 0 {
		t.Fatalf("single volume du returned %d: %s", code, stderr.String())
	}
	var usage volume.Usage
	if err := json.Unmarshal(stdout.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Name != created.Name || usage.TotalBytes != int64(len("single")) {
		t.Fatalf("unexpected single-volume output: %#v", usage)
	}
}
