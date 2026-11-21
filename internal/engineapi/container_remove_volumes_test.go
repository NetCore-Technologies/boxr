package engineapi

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

func TestEngineAPIDeleteWithVolumesRemovesOnlyUnreferencedAnonymousVolumes(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	anonymous, err := volume.CreateAnonymous(nil)
	if err != nil {
		t.Fatal(err)
	}
	named, err := volume.Create("retained-named-volume", nil)
	if err != nil {
		t.Fatal(err)
	}
	newRecord := func(name string, mounts ...container.BindMount) state.Record {
		t.Helper()
		record, err := state.Create(name, []string{"/bin/true"}, container.Options{BindMounts: mounts}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	sharedMount := container.BindMount{Source: anonymous.Path, Target: "/data", VolumeName: anonymous.Name}
	first := newRecord("remove-anonymous-first", sharedMount,
		container.BindMount{Source: named.Path, Target: "/config", VolumeName: named.Name})
	second := newRecord("remove-anonymous-second", sharedMount)

	response := engineAPIRequest(server, http.MethodDelete,
		fmt.Sprintf("/v1.45/containers/%s?v=1", first.ID))
	if response.Code != http.StatusNoContent {
		t.Fatalf("first delete status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := state.Get(first.ID); err == nil {
		t.Fatal("first container remains after delete")
	}
	if _, err := volume.Get(anonymous.Name); err != nil {
		t.Fatalf("anonymous volume still used by second container was removed: %v", err)
	}
	if _, err := volume.Get(named.Name); err != nil {
		t.Fatalf("named volume was removed by Docker -v: %v", err)
	}

	response = engineAPIRequest(server, http.MethodDelete,
		fmt.Sprintf("/v1.45/containers/%s?v=1", second.ID))
	if response.Code != http.StatusNoContent {
		t.Fatalf("second delete status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := volume.Get(anonymous.Name); err == nil {
		t.Fatal("anonymous volume remained after its final container reference was removed")
	}
	if _, err := volume.Get(named.Name); err != nil {
		t.Fatalf("named volume should not be removed by Docker -v: %v", err)
	}
}

func TestEngineAPIDeleteWithoutVolumesPreservesAnonymousVolume(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	item, err := volume.CreateAnonymous(nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := state.Create("keep-anonymous-volume", []string{"/bin/true"}, container.Options{
		BindMounts: []container.BindMount{{Source: item.Path, Target: "/data", VolumeName: item.Name}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodDelete, "/v1.45/containers/"+record.ID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := volume.Get(item.Name); err != nil {
		t.Fatalf("anonymous volume was removed without v=1: %v", err)
	}
}
