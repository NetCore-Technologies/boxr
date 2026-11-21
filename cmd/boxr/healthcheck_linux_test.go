//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestNextContainerHealthStateHonoursGracePeriodAndRetries(t *testing.T) {
	started := time.Now().Add(-time.Second)
	config, err := container.NormalizeHealthcheck(&container.HealthcheckConfig{
		Test: []string{"CMD", "true"}, StartPeriod: 10 * time.Second, Retries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := state.HealthLog{Start: time.Now(), End: time.Now(), ExitCode: 1, Output: "not ready"}
	current := state.HealthState{Status: "starting", Log: []state.HealthLog{}}
	next := nextContainerHealthState(current, &started, config, failure)
	if next.Status != "starting" || next.FailingStreak != 0 {
		t.Fatalf("failure during start period = %+v", next)
	}
	started = time.Now().Add(-20 * time.Second)
	next = nextContainerHealthState(current, &started, config, failure)
	if next.Status != "starting" || next.FailingStreak != 1 {
		t.Fatalf("first failed probe = %+v", next)
	}
	next = nextContainerHealthState(next, &started, config, failure)
	if next.Status != "unhealthy" || next.FailingStreak != 2 {
		t.Fatalf("retry limit not enforced = %+v", next)
	}
}

func TestNextContainerHealthStateRecoversAfterSuccessfulProbe(t *testing.T) {
	config, err := container.NormalizeHealthcheck(&container.HealthcheckConfig{Test: []string{"CMD", "true"}, Retries: 2})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	failure := state.HealthLog{Start: now, End: now, ExitCode: 1}
	success := state.HealthLog{Start: now, End: now, ExitCode: 0}
	current := state.HealthState{Status: "healthy", FailingStreak: 1}
	next := nextContainerHealthState(current, nil, config, success)
	if next.Status != "healthy" || next.FailingStreak != 0 {
		t.Fatalf("successful probe did not restore health: %+v", next)
	}
	next = nextContainerHealthState(state.HealthState{Status: "healthy"}, nil, config, failure)
	if next.Status != "healthy" || next.FailingStreak != 1 {
		t.Fatalf("health changed before retries were exhausted: %+v", next)
	}
}

func TestHealthOutputBufferEnforcesOutputLimit(t *testing.T) {
	output := &healthOutputBuffer{}
	input := strings.Repeat("x", container.MaxHealthcheckOutputBytes+100)
	n, err := output.Write([]byte(input))
	if err != nil || n != len(input) {
		t.Fatalf("bounded write = %d, %v", n, err)
	}
	if got := len(output.String()); got != container.MaxHealthcheckOutputBytes {
		t.Fatalf("health output size = %d", got)
	}
}

func TestHealthcheckProbeOverlappedPause(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BOXR_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(root, "images"))

	created, err := state.Create("health-pause", []string{"/bin/sleep", "30"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(created.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	running, err := state.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Second)
	if err := state.SetPaused(running.ID, running.PID, running.ProcessStartTicks, true); err != nil {
		t.Fatal(err)
	}
	if err := state.SetPaused(running.ID, running.PID, running.ProcessStartTicks, false); err != nil {
		t.Fatal(err)
	}
	overlapped, err := healthcheckProbeOverlappedPause(running.ID, started, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !overlapped {
		t.Fatal("healthcheck probe overlapping a pause/unpause cycle was not detected")
	}
}

func TestHealthcheckProbeWithoutPauseIsNotDiscarded(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BOXR_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(root, "images"))

	created, err := state.Create("health-no-pause", []string{"/bin/sleep", "30"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Second)
	overlapped, err := healthcheckProbeOverlappedPause(created.ID, started, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if overlapped {
		t.Fatal("healthcheck probe without a pause transition was discarded")
	}
}
