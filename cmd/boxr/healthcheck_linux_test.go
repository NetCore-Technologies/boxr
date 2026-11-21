//go:build linux

package main

import (
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
