package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestInspectSizeReportsStateAndLogBytes(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("inspect-size", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	log, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("hello log\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", "--size", record.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect --size returned %d: %s", code, stderr.String())
	}
	var report inspectSizeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode inspect size report: %v: %s", err, stdout.String())
	}
	if report.Record.ID != record.ID || report.Size.StateBytes <= 0 || report.Size.LogBytes != int64(len("hello log\n")) ||
		report.Size.TotalBytes != report.Size.StateBytes+report.Size.LogBytes {
		t.Fatalf("unexpected inspect size report: %#v", report)
	}
}

func TestInspectSizeTreatsMissingLogAsEmpty(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("inspect-no-log", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", "--size", record.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect with missing log returned %d: %s", code, stderr.String())
	}
	var report inspectSizeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Size.StateBytes <= 0 || report.Size.LogBytes != 0 || report.Size.TotalBytes != report.Size.StateBytes {
		t.Fatalf("unexpected missing-log size report: %#v", report.Size)
	}
}

func TestInspectSizeRejectsSymlinkedLog(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("inspect-symlink", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logPath, err := state.LogPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir() + "/external.log"
	if err := os.WriteFile(target, []byte("external data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, logPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", "--size", record.ID}, &stdout, &stderr); code != 1 {
		t.Fatalf("inspect followed symlink log, returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "regular file") {
		t.Fatalf("unexpected symlink rejection: %q", stderr.String())
	}
}
