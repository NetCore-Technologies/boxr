package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

func TestVolumePruneDryRunPreviewsEligibleAndProtectedVolumes(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	unused, err := volume.Create("preview-unused", map[string]string{"team": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := volume.Create("preview-mounted", map[string]string{"team": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.Create("mounting-container", []string{"/bin/true"}, container.Options{
		BindMounts: []container.BindMount{{Source: mounted.Path, Target: "/data"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"volume", "prune", "--dry-run", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("volume prune dry-run returned %d: %s", code, stderr.String())
	}
	var report volumePruneReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode volume prune plan: %v: %s", err, stdout.String())
	}
	if !report.DryRun || len(report.Candidates) != 1 || report.Candidates[0].Name != unused.Name {
		t.Fatalf("unexpected volume candidates: %#v", report)
	}
	if len(report.InUse) != 1 || report.InUse[0].Name != mounted.Name || !strings.Contains(report.InUse[0].Reason, "active container") {
		t.Fatalf("unexpected protected volumes: %#v", report.InUse)
	}
	if _, err := volume.Get(unused.Name); err != nil {
		t.Fatalf("dry-run removed unused volume: %v", err)
	}
	if _, err := volume.Get(mounted.Name); err != nil {
		t.Fatalf("dry-run removed mounted volume: %v", err)
	}
}

func TestVolumePruneDryRunHonorsFiltersAndForceRemovesOnlyUnused(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	alpha, err := volume.Create("alpha", map[string]string{"team": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := volume.Create("beta", map[string]string{"team": "other"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"volume", "prune", "--dry-run", "--filter", "label=team=demo", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("filtered dry-run returned %d: %s", code, stderr.String())
	}
	var report volumePruneReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 1 || report.Candidates[0].Name != alpha.Name {
		t.Fatalf("filter did not select alpha only: %#v", report)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"volume", "prune", "--force", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("volume prune --force returned %d: %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.DryRun || report.RemovedCount != 2 || len(report.Removed) != 2 {
		t.Fatalf("unexpected destructive prune result: %#v", report)
	}
	if _, err := volume.Get(alpha.Name); err == nil {
		t.Fatal("alpha volume was not removed")
	}
	if _, err := volume.Get(beta.Name); err == nil {
		t.Fatal("beta volume was not removed")
	}
}

func TestVolumePruneRequiresForceOrDryRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{
		{"volume", "prune"},
		{"volume", "prune", "--force", "--dry-run"},
	} {
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) returned %d, want 2", args, code)
		}
	}
}


func TestVolumePruneStopsOnUnreadableContainerState(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	stateDir := t.TempDir()
	t.Setenv("BOXR_STATE_DIR", stateDir)
	if err := os.WriteFile(filepath.Join(stateDir, "corrupt.json"), []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := volume.Create("do-not-remove", nil); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"volume", "prune", "--dry-run"}, &stdout, &stderr); code != 1 {
		t.Fatalf("volume prune with unreadable state returned %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "decode state record") {
		t.Fatalf("state error was not surfaced accurately: %q", stderr.String())
	}
	if _, err := volume.Get("do-not-remove"); err != nil {
		t.Fatalf("failed preview unexpectedly removed volume: %v", err)
	}
}
