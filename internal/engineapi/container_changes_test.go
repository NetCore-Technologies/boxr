//go:build linux

package engineapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestScanDockerOverlayChangesReportsAddedChangedAndDeletedPaths(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	write := func(root, name, value string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}

	write(lower, "changed.txt", "base")
	write(lower, "deleted.txt", "remove")
	write(lower, "opaque/old.txt", "old")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	hostSentinel, err := os.CreateTemp(cwd, ".boxr-overlay-lower-sentinel-*")
	if err != nil {
		t.Fatal(err)
	}
	sentinelName := filepath.Base(hostSentinel.Name())
	if err := hostSentinel.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(cwd, sentinelName)) })
	write(upper, "changed.txt", "updated")
	write(upper, "added.txt", "new")
	write(upper, ".wh.deleted.txt", "")
	write(upper, "opaque/.wh..wh..opq", "")
	write(upper, "opaque/new.txt", "new")
	write(upper, "brand-new/.wh."+sentinelName, "")

	changes, err := scanDockerOverlayChanges(lower, upper)
	if err != nil {
		t.Fatalf("scanDockerOverlayChanges: %v", err)
	}
	got := make(map[string]int, len(changes))
	for _, change := range changes {
		got[change.Path] = change.Kind
	}
	want := map[string]int{
		"/changed.txt":   0,
		"/added.txt":     1,
		"/deleted.txt":   2,
		"/opaque":        0,
		"/opaque/new.txt": 1,
		"/opaque/old.txt": 2,
		"/brand-new": 1,
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
	if _, exists := got["/brand-new/"+sentinelName]; exists {
		t.Fatalf("whiteout in a new directory incorrectly inspected the host working directory: %#v", got)
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Errorf("change %s kind = %d, want %d (all changes: %#v)", path, got[path], kind, got)
		}
	}
}

func TestScanDockerOverlayChangesDoesNotFollowSymlinks(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("must not be scanned"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(upper, "linked")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	changes, err := scanDockerOverlayChanges(lower, upper)
	if err != nil {
		t.Fatalf("scanDockerOverlayChanges: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "/linked" || changes[0].Kind != 1 {
		t.Fatalf("symlink changes = %#v, want only added /linked", changes)
	}
}

func TestDockerContainerChangesUsesManagedLayersAndDockerResponseShape(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("changes-test", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootFS, err := state.ManagedRootFSDir(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootFS, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootFS, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ConfigureManagedRootFS(record.ID, rootFS); err != nil {
		t.Fatal(err)
	}
	overlayDir, err := state.ManagedOverlayDir(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(overlayDir, "upper")
	if err := os.MkdirAll(upper, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "created.txt"), []byte("created"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ConfigureManagedOverlay(record.ID); err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodGet, "/v1.45/containers/"+record.ID+"/changes")
	if response.Code != http.StatusOK {
		t.Fatalf("changes status=%d body=%s", response.Code, response.Body.String())
	}
	var changes []struct {
		Path string
		Kind int
	}
	if err := json.Unmarshal(response.Body.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "/created.txt" || changes[0].Kind != 1 {
		t.Fatalf("Docker changes response = %#v", changes)
	}

	response = engineAPIRequest(server, http.MethodGet, "/containers/"+record.ID+"/changes?unexpected=1")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("changes query status=%d, want 400", response.Code)
	}
	response = engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/changes")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST changes status=%d, want 405", response.Code)
	}
}
