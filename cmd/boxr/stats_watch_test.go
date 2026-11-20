package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestStatsWatchEmitsTimestampedJSONLinesAndHonorsCount(t *testing.T) {
	var stdout, stderr bytes.Buffer
	readCalls := 0
	isRunning := func() (bool, error) { return true, nil }
	read := func() (state.Stats, error) {
		readCalls++
		return state.Stats{PID: 123, MemoryCurrent: int64(readCalls * 1024)}, nil
	}

	if code := streamStats(&stdout, &stderr, time.Millisecond, 2, "json", isRunning, read); code != 0 {
		t.Fatalf("streamStats returned %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || readCalls != 2 {
		t.Fatalf("got %d output lines and %d reads, want 2 each: %q", len(lines), readCalls, stdout.String())
	}
	for index, line := range lines {
		var sample statsWatchSample
		if err := json.Unmarshal([]byte(line), &sample); err != nil {
			t.Fatalf("decode sample %d: %v", index, err)
		}
		if sample.SampledAt.IsZero() || sample.Stats.PID != 123 || sample.Stats.MemoryCurrent != int64((index+1)*1024) {
			t.Errorf("unexpected sample %d: %#v", index, sample)
		}
	}
}

func TestStatsWatchStopsCleanlyWhenContainerExits(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runningChecks := 0
	readCalls := 0
	isRunning := func() (bool, error) {
		runningChecks++
		return runningChecks == 1, nil
	}
	read := func() (state.Stats, error) {
		readCalls++
		return state.Stats{PID: 88}, nil
	}

	if code := streamStats(&stdout, &stderr, time.Millisecond, 0, "json", isRunning, read); code != 0 {
		t.Fatalf("streamStats returned %d: %s", code, stderr.String())
	}
	if readCalls != 1 || len(strings.Split(strings.TrimSpace(stdout.String()), "\n")) != 1 {
		t.Fatalf("watch did not stop after the container exited: reads=%d output=%q", readCalls, stdout.String())
	}
}

func TestStatsWatchReportsCgroupAndOutputErrors(t *testing.T) {
	var stderr bytes.Buffer
	isRunning := func() (bool, error) { return true, nil }
	readFailure := func() (state.Stats, error) { return state.Stats{}, errors.New("cgroup disappeared") }
	if code := streamStats(&bytes.Buffer{}, &stderr, time.Millisecond, 0, "json", isRunning, readFailure); code != 1 {
		t.Fatalf("cgroup read failure returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cgroup disappeared") {
		t.Fatalf("missing cgroup error: %q", stderr.String())
	}

	stderr.Reset()
	writeFailure := func() (state.Stats, error) { return state.Stats{PID: 77}, nil }
	if code := streamStats(statsFailWriter{}, &stderr, time.Millisecond, 0, "json", isRunning, writeFailure); code != 1 {
		t.Fatalf("output failure returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "write sample") {
		t.Fatalf("missing output error: %q", stderr.String())
	}
}

type statsFailWriter struct{}

func (statsFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}

func TestRunStatsRejectsInvalidWatchOptions(t *testing.T) {
	for _, args := range [][]string{
		{"stats", "--watch", "--interval", "1ms", "demo"},
		{"stats", "--watch", "--count", "-1", "demo"},
		{"stats", "--count", "2", "demo"},
		{"stats", "--watch", "--interval", "0s", "demo"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) returned %d, want 2; stderr=%q", args, code, stderr.String())
		}
	}
}

func TestStatsWatchRejectsCreatedContainer(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("stats-created", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"stats", "--watch", record.ID}, &stdout, &stderr); code != 1 {
		t.Fatalf("stats watch for created container returned %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Fatalf("unexpected error: %q", stderr.String())
	}
}


func TestStatsTableOneShotAndWatchOutput(t *testing.T) {
	stats := state.Stats{PID: 321, MemoryCurrent: 4096, MemoryMax: "8192", PIDsCurrent: 5, CPUUsageUsec: 900, CPUUserUsec: 600, CPUSystemUsec: 300}
	var oneShot bytes.Buffer
	if err := writeStatsTable(&oneShot, time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC), stats); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(oneShot.String(), "MEMORY_CURRENT_B") || !strings.Contains(oneShot.String(), "4096") || !strings.Contains(oneShot.String(), "900") {
		t.Fatalf("table missing resource values: %q", oneShot.String())
	}

	var stdout, stderr bytes.Buffer
	isRunning := func() (bool, error) { return true, nil }
	read := func() (state.Stats, error) { return stats, nil }
	if code := streamStats(&stdout, &stderr, time.Millisecond, 2, "table", isRunning, read); code != 0 {
		t.Fatalf("table watch returned %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || lines[0] != statsTableHeader {
		t.Fatalf("table watch should emit one header and two samples: %q", stdout.String())
	}
}
