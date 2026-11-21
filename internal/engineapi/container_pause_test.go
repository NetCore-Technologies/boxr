package engineapi

import (
	"errors"
	"net/http"
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

	select {
	case transition := <-transitions:
		t.Fatalf("second transition reached freezer before pause committed: %t", transition)
	case <-time.After(25 * time.Millisecond):
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
