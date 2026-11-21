package state

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
)

func useTempState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BOXR_STATE_DIR", dir)
	return dir
}

func TestCreateStartFinishInspectAndRemoveLifecycle(t *testing.T) {
	dir := useTempState(t)
	record, err := Create("api-test", []string{"/bin/true"}, container.Options{Env: []string{"TOKEN=secret"}}, map[string]string{"team": "runtime"})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "created" || len(record.ID) != 12 {
		t.Fatalf("bad new record: %#v", record)
	}
	if err := Start(record.ID, 4321); err != nil {
		t.Fatal(err)
	}
	if err := Finish(record.ID, &container.ExitError{Code: 7}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Get("api-test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "failed" || loaded.PID != 4321 || loaded.ExitCode == nil || *loaded.ExitCode != 7 {
		t.Fatalf("bad finished record: %#v", loaded)
	}
	if len(loaded.Options.Env) != 0 {
		t.Fatalf("environment values leaked into state: %#v", loaded.Options.Env)
	}
	if info, err := os.Stat(filepath.Join(dir, record.ID+".json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file permissions: %v %v", info, err)
	}
	if err := Remove(record.ID[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(record.ID); err == nil {
		t.Fatal("record still exists after removal")
	}
	events, err := Events(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d lifecycle events, want create/start/die/remove", len(events))
	}
}

func TestManagedOverlayIsPrivateAndRemovedWithContainer(t *testing.T) {
	stateDir := useTempState(t)
	record, err := Create("managed-overlay", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := ConfigureManagedOverlay(record.ID)
	if err != nil {
		t.Fatalf("configure managed writable layer: %v", err)
	}
	expected := filepath.Join(stateDir, "writable", record.ID)
	if configured.Options.OverlayDir != expected {
		t.Fatalf("OverlayDir=%q, want %q", configured.Options.OverlayDir, expected)
	}
	parentInfo, err := os.Stat(filepath.Dir(expected))
	if err != nil {
		t.Fatal(err)
	}
	if parentInfo.Mode().Perm() != 0700 {
		t.Fatalf("writable-layer parent permissions=%o, want 0700", parentInfo.Mode().Perm())
	}
	for _, directory := range []string{"upper", "work", "merged"} {
		if err := os.MkdirAll(filepath.Join(expected, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(expected, "upper", "persisted.txt"), []byte("persistent"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCreated(record.ID); err != nil {
		t.Fatalf("remove Engine API container: %v", err)
	}
	if _, err := os.Lstat(expected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed writable layer remains after container removal: %v", err)
	}
}

func TestContainerRemovalNeverDeletesUserManagedOverlay(t *testing.T) {
	useTempState(t)
	userOverlay := filepath.Join(t.TempDir(), "user-overlay")
	if err := os.MkdirAll(filepath.Join(userOverlay, "upper"), 0700); err != nil {
		t.Fatal(err)
	}
	record, err := Create("user-overlay", []string{"/bin/true"}, container.Options{OverlayDir: userOverlay}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveCreated(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(userOverlay, "upper")); err != nil {
		t.Fatalf("user-managed OverlayFS data was removed: %v", err)
	}
}

func TestCreateRejectsDuplicateNamesAndInvalidNames(t *testing.T) {
	useTempState(t)
	if _, err := Create("web", []string{"sh"}, container.Options{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Create("web", []string{"sh"}, container.Options{}, nil); err == nil {
		t.Fatal("duplicate name accepted")
	}
	for _, name := range []string{"../escape", "bad name", "", "a/b"} {
		if name == "" {
			continue
		}
		if _, err := Create(name, []string{"sh"}, container.Options{}, nil); err == nil {
			t.Errorf("invalid name %q accepted", name)
		}
	}
}

func TestRunningContainerCannotBeRemoved(t *testing.T) {
	useTempState(t)
	record, err := Create("running", []string{"sh"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(record.ID, 44); err != nil {
		t.Fatal(err)
	}
	if err := Remove(record.ID); err == nil {
		t.Fatal("running container removed")
	}
}

func TestFindDetectsAmbiguousPrefix(t *testing.T) {
	useTempState(t)
	first, err := Create("first", []string{"sh"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Create("second", []string{"sh"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Make the records share a prefix to exercise the ambiguity guard.
	originalID := second.ID
	second.ID = first.ID[:3] + "abcdefghi"
	if err := os.Remove(recordPath(originalID)); err != nil {
		t.Fatal(err)
	}
	if err := saveRecord(second); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(first.ID[:3]); err == nil {
		t.Fatal("ambiguous prefix resolved to one container")
	}
}

func TestListFiltersAndPrunePreservesRunningRecords(t *testing.T) {
	useTempState(t)
	completed, err := Create("completed", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(completed.ID, 100); err != nil {
		t.Fatal(err)
	}
	if err := Finish(completed.ID, nil); err != nil {
		t.Fatal(err)
	}
	running, err := Create("active", []string{"/bin/sleep", "30"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(running.ID, 101); err != nil {
		t.Fatal(err)
	}
	active, err := List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != running.ID {
		t.Fatalf("active list = %#v", active)
	}
	removed, err := Prune(time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("pruned %d records, want 1", removed)
	}
	if _, err := Get(running.ID); err != nil {
		t.Fatalf("running record was pruned: %v", err)
	}
	if _, err := Get(completed.ID); err == nil {
		t.Fatal("completed record remains after prune")
	}
}

func TestEventTailLimit(t *testing.T) {
	useTempState(t)
	for _, name := range []string{"a", "b", "c"} {
		record, err := Create(name, []string{"sh"}, container.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := Start(record.ID, 123); err != nil {
			t.Fatal(err)
		}
		if err := Finish(record.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	events, err := Events(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Action != "start" || events[1].Action != "die" {
		t.Fatalf("unexpected event tail: %#v", events)
	}
}

func TestFinishPreservesWorkloadExitCode(t *testing.T) {
	useTempState(t)
	record, err := Create("nonzero", []string{"sh", "-c", "exit 7"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(record.ID, 404); err != nil {
		t.Fatal(err)
	}
	processErr := exec.Command("sh", "-c", "exit 7").Run()
	if processErr == nil {
		t.Fatal("test command unexpectedly succeeded")
	}
	if err := Finish(record.ID, processErr); err != nil {
		t.Fatal(err)
	}
	loaded, err := Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExitCode == nil || *loaded.ExitCode != 7 || loaded.Status != "failed" || loaded.Error != "" {
		t.Fatalf("workload exit status not preserved: %#v", loaded)
	}
}

func TestStateDirectoryRejectsSymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOXR_STATE_DIR", link)
	if _, err := List(true); err == nil {
		t.Fatal("state directory symlink unexpectedly accepted")
	}
}

func TestStateRecordRejectsSymlink(t *testing.T) {
	dir := useTempState(t)
	external := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(external, []byte(`{"id":"outside"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := List(true); err == nil {
		t.Fatal("symlinked state record unexpectedly accepted")
	}
}

func TestEventJournalRejectsSymlink(t *testing.T) {
	dir := useTempState(t)
	external := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(external, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Events(0); err == nil {
		t.Fatal("symlinked event journal unexpectedly accepted")
	}
	if _, err := Create("must-rollback", []string{"true"}, container.Options{}, nil); err == nil {
		t.Fatal("create unexpectedly appended to symlinked event journal")
	}
	if _, err := os.Stat(recordPath("must-rollback")); !os.IsNotExist(err) {
		t.Fatalf("failed create left state record behind: %v", err)
	}
}

func TestRestartLifecyclePersistsAttemptsAndManualStopIntent(t *testing.T) {
	useTempState(t)
	record, err := Create("restart-test", []string{"/bin/false"}, container.Options{
		RestartPolicy: "on-failure", RestartMaxRetries: 2, RestartDelayMS: 250,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(record.ID, 4321); err != nil {
		t.Fatal(err)
	}
	if err := Finish(record.ID, &container.ExitError{Code: 7}); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRestart(record.ID); err != nil {
		t.Fatalf("PrepareRestart(): %v", err)
	}
	restarting, err := Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restarting.Status != "restarting" || restarting.RestartCount != 1 ||
		restarting.ExitCode == nil || *restarting.ExitCode != 7 ||
		restarting.LastExitCode == nil || *restarting.LastExitCode != 7 || restarting.PID != 0 {
		t.Fatalf("restart record = %+v", restarting)
	}
	items, err := List(false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.ID == record.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("restarting record is missing from active container listing")
	}
	if err := Start(record.ID, 4322); err != nil {
		t.Fatalf("Start(restart attempt): %v", err)
	}
	if err := RequestStop(record.ID); err != nil {
		t.Fatalf("RequestStop(): %v", err)
	}
	if err := RequestStop(record.ID); err != nil {
		t.Fatalf("repeated RequestStop(): %v", err)
	}
	stopping, err := Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stopping.StopRequested || stopping.Status != "running" {
		t.Fatalf("stop intent was not persisted on running record: %+v", stopping)
	}
	if err := Finish(record.ID, &container.ExitError{Code: 143}); err != nil {
		t.Fatal(err)
	}
	stopped, err := Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped.StopRequested || stopped.Status != "failed" {
		t.Fatalf("finished manual stop state = %+v", stopped)
	}
	if err := PrepareRestart(record.ID); err == nil {
		t.Fatal("restart was prepared despite a persisted stop request")
	}
}

func TestRequestStopCancelsPendingRestartWithoutSignallingPID(t *testing.T) {
	useTempState(t)
	record, err := Create("restart-cancel", []string{"/bin/true"}, container.Options{RestartPolicy: "always"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(record.ID, 4323); err != nil {
		t.Fatal(err)
	}
	if err := Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRestart(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := RequestStop(record.ID); err != nil {
		t.Fatalf("RequestStop(restarting): %v", err)
	}
	loaded, err := Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "exited" || loaded.PID != 0 || !loaded.StopRequested || loaded.RestartCount != 1 {
		t.Fatalf("cancelled restart state = %+v", loaded)
	}
}

func TestRemoveRefusesContainerDuringRestartBackoff(t *testing.T) {
	useTempState(t)
	record, err := Create("remove-restarting", []string{"/bin/true"}, container.Options{RestartPolicy: "always"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(record.ID, 5701); err != nil {
		t.Fatal(err)
	}
	if err := Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRestart(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := Remove(record.ID); err == nil || !strings.Contains(err.Error(), "restarting") {
		t.Fatalf("Remove() during restart backoff error = %v, want restarting guard", err)
	}
	if err := RequestStop(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := Remove(record.ID); err != nil {
		t.Fatalf("Remove() after restart cancellation: %v", err)
	}
}

func TestEngineConfigIsStoredPrivatelyAndRemovedWithContainer(t *testing.T) {
	directory := useTempState(t)
	record, err := Create("engine-config-test", []string{"/bin/true"}, container.Options{
		Env: []string{"TOKEN=secret"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"version":1,"options":{"env":["TOKEN=secret"]}}`)
	if err := SaveEngineConfig(record.ID, payload); err != nil {
		t.Fatalf("SaveEngineConfig: %v", err)
	}
	loaded, err := ReadEngineConfig(record.ID)
	if err != nil {
		t.Fatalf("ReadEngineConfig: %v", err)
	}
	if string(loaded) != string(payload) {
		t.Fatalf("loaded config = %q, want %q", loaded, payload)
	}
	info, err := os.Stat(filepath.Join(directory, "engine-config", record.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("Engine API config mode = %o, want 0600", info.Mode().Perm())
	}
	configDirectory, err := os.Stat(filepath.Join(directory, "engine-config"))
	if err != nil {
		t.Fatal(err)
	}
	if configDirectory.Mode().Perm() != 0700 {
		t.Fatalf("Engine API config directory mode = %o, want 0700", configDirectory.Mode().Perm())
	}
	if err := RemoveCreated(record.ID); err != nil {
		t.Fatalf("RemoveCreated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "engine-config", record.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("Engine API config survived container removal: %v", err)
	}
}

func TestEngineConfigRejectsOversizedAndUnsafeFiles(t *testing.T) {
	directory := useTempState(t)
	record, err := Create("engine-config-unsafe", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveEngineConfig(record.ID, make([]byte, maxEngineConfigBytes+1)); err == nil {
		t.Fatal("oversized Engine API configuration was accepted")
	}
	configDirectory := filepath.Join(directory, "engine-config")
	if err := os.Mkdir(configDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDirectory, record.ID+".json")
	if err := os.Symlink(filepath.Join(directory, "target"), configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEngineConfig(record.ID); err == nil {
		t.Fatal("symlinked Engine API configuration was read")
	}
	if err := RemoveCreated(record.ID); err == nil {
		t.Fatal("container removal accepted a symlinked Engine API config")
	}
	if _, err := os.Lstat(configPath); err != nil {
		t.Fatalf("failed removal unexpectedly deleted symlink: %v", err)
	}
}

func TestRenamePersistsUniqueContainerNamesAndEvents(t *testing.T) {
	useTempState(t)
	first, err := Create("before-rename", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Create("already-used", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	renamed, err := Rename(first.ID, "after-rename")
	if err != nil {
		t.Fatalf("Rename(): %v", err)
	}
	if renamed.ID != first.ID || renamed.Name != "after-rename" {
		t.Fatalf("renamed record = %+v", renamed)
	}
	if _, err := Get("before-rename"); err == nil {
		t.Fatal("old container name still resolves after rename")
	}
	loaded, err := Get("after-rename")
	if err != nil || loaded.ID != first.ID {
		t.Fatalf("new container name lookup = %+v, err=%v", loaded, err)
	}

	if _, err := Rename(first.ID, "already-used"); err == nil {
		t.Fatal("duplicate container name was accepted")
	}
	for _, invalid := range []string{"", "../escape", "contains space", strings.Repeat("x", 65)} {
		if _, err := Rename(first.ID, invalid); err == nil {
			t.Errorf("invalid container name %q was accepted", invalid)
		}
	}
	if _, err := Rename(first.ID, "after-rename"); err != nil {
		t.Fatalf("renaming to the existing name should be idempotent: %v", err)
	}

	events, err := Events(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.ID == first.ID && event.Action == "rename" && event.Name == "after-rename" {
			found = true
		}
	}
	if !found {
		t.Fatal("container rename event was not persisted")
	}
	if _, err := Get(second.ID); err != nil {
		t.Fatalf("unrelated container record changed: %v", err)
	}
}
