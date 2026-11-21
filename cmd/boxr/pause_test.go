package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func createCLIPauseTestContainer(t *testing.T, name string) state.Record {
	t.Helper()
	t.Setenv("BOXR_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	record, err := state.Create(name, []string{"/bin/sleep", "30"}, container.Options{CgroupEnabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	running, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.ProcessStartTicks == 0 {
		t.Skip("current platform does not expose process start ticks")
	}
	return running
}

func TestTransitionContainerPauseFreezesAndPersistsBothDirections(t *testing.T) {
	record := createCLIPauseTestContainer(t, "cli-pause")
	var transitions []bool
	setFrozen := func(pid int, frozen bool) error {
		if pid != record.PID {
			t.Fatalf("freezer PID = %d, want %d", pid, record.PID)
		}
		transitions = append(transitions, frozen)
		return nil
	}
	if err := transitionContainerPause(record, true, setFrozen); err != nil {
		t.Fatalf("pause: %v", err)
	}
	paused, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		t.Fatalf("state after pause = %q", paused.Status)
	}
	if err := transitionContainerPause(paused, false, setFrozen); err != nil {
		t.Fatalf("unpause: %v", err)
	}
	resumed, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "running" {
		t.Fatalf("state after unpause = %q", resumed.Status)
	}
	if len(transitions) != 2 || !transitions[0] || transitions[1] {
		t.Fatalf("cgroup transitions = %v, want [true false]", transitions)
	}
}

func TestTransitionContainerPauseRollsBackOnStateRace(t *testing.T) {
	record := createCLIPauseTestContainer(t, "cli-pause-race")
	var transitions []bool
	setFrozen := func(_ int, frozen bool) error {
		transitions = append(transitions, frozen)
		if frozen {
			return state.RequestStop(record.ID)
		}
		return nil
	}
	if err := transitionContainerPause(record, true, setFrozen); err == nil {
		t.Fatal("pause succeeded after a concurrent stop request")
	}
	current, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "running" || !current.StopRequested {
		t.Fatalf("concurrent stop state was not preserved: %+v", current)
	}
	if len(transitions) != 2 || !transitions[0] || transitions[1] {
		t.Fatalf("freezer rollback sequence = %v, want [true false]", transitions)
	}
	if err := state.CancelStopRequest(record.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTransitionContainerPauseRejectsStaleProcessIdentityBeforeFreezing(t *testing.T) {
	record := createCLIPauseTestContainer(t, "cli-pause-stale")
	record.ProcessStartTicks++
	calls := 0
	err := transitionContainerPause(record, true, func(int, bool) error {
		calls++
		return nil
	})
	if err == nil {
		t.Fatal("pause accepted stale process identity")
	}
	if calls != 0 {
		t.Fatalf("freezer was called %d times for a stale process", calls)
	}
}

func TestRunPauseAndUnpauseRejectMissingArguments(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := runPause(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("runPause() = %d, want 2", code)
	}
	if code := runUnpause(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("runUnpause() = %d, want 2", code)
	}
}
