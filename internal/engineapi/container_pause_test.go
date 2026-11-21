package engineapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestDockerContainerPauseAndUnpauseFreezeAndPersistState(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "pause-test", container.Options{CgroupEnabled: true})
	running, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	server.verifyUpdatePID = func(state.Record) error { return nil }
	var transitions []bool
	server.setCgroupFrozen = func(pid int, frozen bool) error {
		if pid != running.PID {
			t.Fatalf("freezer PID = %d, want %d", pid, running.PID)
		}
		transitions = append(transitions, frozen)
		return nil
	}

	response := engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/pause")
	if response.Code != http.StatusNoContent {
		t.Fatalf("pause status=%d body=%s", response.Code, response.Body.String())
	}
	paused, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		t.Fatalf("state after pause = %q", paused.Status)
	}

	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/unpause")
	if response.Code != http.StatusNoContent {
		t.Fatalf("unpause status=%d body=%s", response.Code, response.Body.String())
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

	events, err := state.Events(20)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, event := range events {
		if event.ID == record.ID {
			actions = append(actions, event.Action)
		}
	}
	if len(actions) < 3 || actions[len(actions)-2] != "pause" || actions[len(actions)-1] != "unpause" {
		t.Fatalf("pause lifecycle events = %v", actions)
	}
}

func TestDockerContainerPauseRollsBackWhenCgroupFreezeFails(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "pause-failure", container.Options{CgroupEnabled: true})
	server.verifyUpdatePID = func(state.Record) error { return nil }
	server.setCgroupFrozen = func(int, bool) error { return errors.New("freezer controller unavailable") }

	response := engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/pause")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("pause status=%d body=%s", response.Code, response.Body.String())
	}
	current, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "running" {
		t.Fatalf("failed pause changed persisted state to %q", current.Status)
	}
}

func TestDockerContainerPauseRejectsUnverifiedContainerBeforeFreezing(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "pause-unverified", container.Options{CgroupEnabled: true})
	server.verifyUpdatePID = func(state.Record) error { return errors.New("not Boxr-owned") }
	calls := 0
	server.setCgroupFrozen = func(int, bool) error {
		calls++
		return nil
	}

	response := engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/pause")
	if response.Code != http.StatusConflict {
		t.Fatalf("pause status=%d body=%s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("freezer was called %d times for an unverified container", calls)
	}
}

func TestDockerContainerPauseRejectsQueriesAndInvalidStates(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("pause-created", []string{"/bin/true"}, container.Options{CgroupEnabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.setCgroupFrozen = func(int, bool) error {
		t.Fatal("freezer called for a container that is not running")
		return nil
	}
	for _, test := range []struct {
		target string
		want   int
	}{
		{target: "/containers/" + record.ID + "/pause?unexpected=1", want: http.StatusBadRequest},
		{target: "/containers/" + record.ID + "/pause", want: http.StatusConflict},
	} {
		response := engineAPIRequest(server, http.MethodPost, test.target)
		if response.Code != test.want {
			t.Errorf("%s: status=%d, want %d", test.target, response.Code, test.want)
		}
	}
	response := engineAPIRequest(server, http.MethodGet, "/containers/"+record.ID+"/pause")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET pause status=%d, want 405", response.Code)
	}
}

func TestDockerContainerPauseAndUnpauseSerializePerContainer(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "pause-concurrency", container.Options{CgroupEnabled: true})
	server.verifyUpdatePID = func(state.Record) error { return nil }

	freezeStarted := make(chan struct{})
	releaseFreeze := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFreeze) }) }
	defer release()
	transitions := make(chan bool, 2)
	server.setCgroupFrozen = func(_ int, frozen bool) error {
		transitions <- frozen
		if frozen {
			close(freezeStarted)
			<-releaseFreeze
		}
		return nil
	}

	pauseResponse := make(chan int, 1)
	go func() {
		pauseResponse <- engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/pause").Code
	}()
	select {
	case <-freezeStarted:
	case <-time.After(time.Second):
		t.Fatal("pause did not reach the cgroup freezer")
	}

	unpauseStarted := make(chan struct{})
	unpauseResponse := make(chan int, 1)
	go func() {
		close(unpauseStarted)
		unpauseResponse <- engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/unpause").Code
	}()
	<-unpauseStarted

	deadline := time.Now().Add(time.Second)
	for {
		server.containerTransitions.mu.Lock()
		entry := server.containerTransitions.locks[record.ID]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		server.containerTransitions.mu.Unlock()
		if refs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unpause did not register as a waiter for the container transition lock")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case transition := <-transitions:
		t.Fatalf("second transition reached freezer before pause committed: %t", transition)
	default:
	}

	release()
	if got := <-pauseResponse; got != http.StatusNoContent {
		t.Fatalf("pause status=%d, want %d", got, http.StatusNoContent)
	}
	if got := <-unpauseResponse; got != http.StatusNoContent {
		t.Fatalf("unpause status=%d, want %d", got, http.StatusNoContent)
	}

	first, second := <-transitions, <-transitions
	if first || !second {
		t.Fatalf("serialized freezer transitions = [%t %t], want [true false]", first, second)
	}
	current, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "running" {
		t.Fatalf("final container state=%q, want running", current.Status)
	}
}

func TestResumePausedContainerThawsAndPersistsRunningState(t *testing.T) {
	server := setupEngineAPITest(t)
	created := createEngineAPITestContainer(t, "resume-helper", container.Options{CgroupEnabled: true})
	running, err := state.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetPaused(running.ID, running.PID, running.ProcessStartTicks, true); err != nil {
		t.Fatal(err)
	}
	paused, err := state.Get(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	server.verifyUpdatePID = func(state.Record) error { return nil }
	var transitions []bool
	server.setCgroupFrozen = func(pid int, frozen bool) error {
		if pid != running.PID {
			t.Fatalf("freezer PID = %d, want %d", pid, running.PID)
		}
		transitions = append(transitions, frozen)
		return nil
	}
	if err := server.resumePausedContainer(paused); err != nil {
		t.Fatalf("resumePausedContainer(): %v", err)
	}
	resumed, err := state.Get(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "running" || len(transitions) != 1 || transitions[0] {
		t.Fatalf("resumed state=%+v transitions=%v", resumed, transitions)
	}
}

func TestResumePausedContainerThawsBackOnFailedStatePersistence(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BOXR_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(root, "images"))
	server := New("test-version")
	created, err := state.Create("resume-rollback", []string{"/bin/sleep", "30"}, container.Options{CgroupEnabled: true}, nil)
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
	if err := state.SetPaused(running.ID, running.PID, running.ProcessStartTicks, true); err != nil {
		t.Fatal(err)
	}
	paused, err := state.Get(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(root, "state", "events.jsonl")
	if err := os.Rename(eventsPath, eventsPath+".saved"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(target, []byte("leave untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, eventsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	server.verifyUpdatePID = func(state.Record) error { return nil }
	var transitions []bool
	server.setCgroupFrozen = func(_ int, frozen bool) error {
		transitions = append(transitions, frozen)
		return nil
	}
	if err := server.resumePausedContainer(paused); err == nil {
		t.Fatal("resume succeeded with an unsafe event log path")
	}
	current, err := state.Get(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "paused" || len(transitions) != 2 || transitions[0] || !transitions[1] {
		t.Fatalf("rollback state=%+v transitions=%v", current, transitions)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "leave untouched" {
		t.Fatalf("event target changed: %q, %v", got, err)
	}
}

func TestContainerListIncludesPausedContainersByDefault(t *testing.T) {
	server := setupEngineAPITest(t)
	created := createEngineAPITestContainer(t, "pause-list", container.Options{CgroupEnabled: true})
	running, err := state.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetPaused(running.ID, running.PID, running.ProcessStartTicks, true); err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodGet, "/containers/json")
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["State"] != "paused" {
		t.Fatalf("default container list omitted paused workload: %#v", items)
	}
}
