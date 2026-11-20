package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestPruneDryRunListsOnlyEligibleRecordsWithoutMutation(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	completed, err := state.Create("prune-completed", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(completed.ID, 30001); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(completed.ID, nil); err != nil {
		t.Fatal(err)
	}

	running, err := state.Create("prune-running", []string{"/bin/sleep", "1"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(running.ID, 30002); err != nil {
		t.Fatal(err)
	}
	created, err := state.Create("prune-created", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"prune", "--older-than", "0s", "--dry-run", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("prune dry-run returned %d: %s", code, stderr.String())
	}
	var report pruneReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode prune plan: %v: %s", err, stdout.String())
	}
	if !report.DryRun || report.Count != 1 || len(report.Candidates) != 1 || report.Candidates[0].ID != completed.ID {
		t.Fatalf("unexpected prune plan: %#v", report)
	}
	for _, id := range []string{completed.ID, running.ID, created.ID} {
		if _, err := state.Get(id); err != nil {
			t.Errorf("dry-run removed %s: %v", id, err)
		}
	}
}

func TestPruneDryRunHumanOutputAndJSONCleanupResult(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("prune-report", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 30003); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"prune", "--older-than", "0s", "--dry-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("prune dry-run returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "would prune 1") || !strings.Contains(stdout.String(), record.ID) {
		t.Fatalf("dry-run did not list candidate: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"prune", "--older-than", "0s", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("prune JSON returned %d: %s", code, stderr.String())
	}
	var result pruneReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode prune result: %v", err)
	}
	if result.DryRun || result.Removed != 1 || result.Count != 1 {
		t.Fatalf("unexpected cleanup result: %#v", result)
	}
	if _, err := state.Get(record.ID); err == nil {
		t.Fatal("mutating prune left the completed record")
	}
}
