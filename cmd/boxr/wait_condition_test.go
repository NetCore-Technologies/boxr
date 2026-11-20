package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestWaitConditionRunningReportsJSONReadiness(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("wait-ready", []string{"/bin/sleep", "1"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		started <- state.Start(record.ID, 12345)
	}()

	var stdout, stderr bytes.Buffer
	code := run([]string{"wait", "--condition", "running", "--interval", "10ms", "--timeout", "1s", "--json", record.ID}, &stdout, &stderr)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("wait for running returned %d: %s", code, stderr.String())
	}
	var results []waitResult
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("decode wait JSON: %v: %q", err, stdout.String())
	}
	if len(results) != 1 || results[0].ID != record.ID || results[0].Condition != "running" || results[0].Status != "running" {
		t.Fatalf("unexpected readiness result: %#v", results)
	}
}

func TestWaitConditionStoppedPreservesExitCodeOutput(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("wait-stop", []string{"/bin/sleep", "1"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 12346); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		finished <- state.Finish(record.ID, nil)
	}()

	var stdout, stderr bytes.Buffer
	code := run([]string{"wait", "--interval", "10ms", "--timeout", "1s", record.ID}, &stdout, &stderr)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if code != 0 || strings.TrimSpace(stdout.String()) != "0" {
		t.Fatalf("legacy wait result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestWaitConditionTimeoutAndPrematureExit(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	created, err := state.Create("wait-timeout", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"wait", "--condition", "running", "--timeout", "10ms", "--interval", "10ms", created.ID}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "timed out") {
		t.Fatalf("timeout returned code=%d stderr=%q", code, stderr.String())
	}

	failed, err := state.Create("wait-failed", []string{"/bin/false"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(failed.ID, fmt.Errorf("failed before start")); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	code = run([]string{"wait", "--condition", "running", "--timeout", "100ms", failed.ID}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "before becoming running") {
		t.Fatalf("premature exit returned code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunWaitRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{
		{"wait", "--condition", "healthy", "demo"},
		{"wait", "--condition", "running", "--interval", "1ms", "demo"},
		{"wait", "--timeout", "-1s", "demo"},
		{"wait", "--interval", "0s", "demo"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) returned %d, want 2; stderr=%q", args, code, stderr.String())
		}
	}
}
