package engineapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

func TestDockerVolumeMountCopiesImageDataByDefaultAndHonoursNoCopy(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	image := importEngineAPITestImage(t, "registry.example/team/volume-copy-up:v1")
	tests := []struct {
		name       string
		volumeName string
		mount      string
		wantCopy   bool
	}{
		{
			name:       "default copy-up",
			volumeName: "default-copy-up",
			mount:      `{"Type":"volume","Source":"default-copy-up","Target":"/bin"}`,
			wantCopy:   true,
		},
		{
			name:       "explicit copy-up",
			volumeName: "explicit-copy-up",
			mount:      `{"Type":"volume","Source":"explicit-copy-up","Target":"/bin","VolumeOptions":{"NoCopy":false}}`,
			wantCopy:   true,
		},
		{
			name:       "NoCopy",
			volumeName: "no-copy",
			mount:      `{"Type":"volume","Source":"no-copy","Target":"/bin","VolumeOptions":{"NoCopy":true}}`,
			wantCopy:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, err := volume.Create(test.volumeName, nil)
			if err != nil {
				t.Fatal(err)
			}
			record, err := createContainerRecord(test.volumeName, dockerCreateRequest{
				Image: image.Reference,
				HostConfig: dockerHostConfig{Mounts: []json.RawMessage{
					json.RawMessage(test.mount),
				}},
			})
			if err != nil {
				t.Fatalf("create Docker container: %v", err)
			}
			t.Cleanup(func() {
				if err := state.RemoveCreated(record.ID); err != nil {
					t.Errorf("remove created Engine API container: %v", err)
				}
			})
			mount := record.Options.BindMounts
			if len(mount) != 1 || mount[0].VolumeName != test.volumeName || mount[0].CopyUp != test.wantCopy {
				t.Fatalf("saved volume mount metadata = %+v", mount)
			}
			data, readErr := os.ReadFile(filepath.Join(item.Path, "app"))
			if test.wantCopy {
				if readErr != nil || string(data) != "boxr test image\n" {
					t.Fatalf("copied image file = %q, err=%v", data, readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("NoCopy unexpectedly populated volume: data=%q err=%v", data, readErr)
			}
		})
	}
}

func TestDockerLegacyNamedVolumeBindUsesDefaultCopyUp(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	item, err := volume.Create("legacy-copy-up", nil)
	if err != nil {
		t.Fatal(err)
	}
	image := importEngineAPITestImage(t, "registry.example/team/legacy-volume-copy-up:v1")
	record, err := createContainerRecord("legacy-copy-up-container", dockerCreateRequest{
		Image: image.Reference,
		HostConfig: dockerHostConfig{Binds: []string{"legacy-copy-up:/bin"}},
	})
	if err != nil {
		t.Fatalf("create Docker container from legacy volume bind: %v", err)
	}
	t.Cleanup(func() {
		if err := state.RemoveCreated(record.ID); err != nil {
			t.Errorf("remove created Engine API container: %v", err)
		}
	})
	data, err := os.ReadFile(filepath.Join(item.Path, "app"))
	if err != nil || string(data) != "boxr test image\n" {
		t.Fatalf("legacy bind copy-up = %q, err=%v", data, err)
	}
}

func TestDockerVolumeCopyUpRefusesToMutateVolumeUsedByRunningContainer(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	item, err := volume.Create("active-volume", nil)
	if err != nil {
		t.Fatal(err)
	}
	active, err := state.Create("active-volume-user", []string{"/bin/app"}, container.Options{
		BindMounts: []container.BindMount{{Source: item.Path, Target: "/data"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(active.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Finish(active.ID, nil); err != nil {
			t.Errorf("finish simulated active container: %v", err)
		}
		if err := state.Remove(active.ID); err != nil {
			t.Errorf("remove simulated active container: %v", err)
		}
	})
	image := importEngineAPITestImage(t, "registry.example/team/active-volume-copy-up:v1")
	_, err = createContainerRecord("second-volume-user", dockerCreateRequest{
		Image: image.Reference,
		HostConfig: dockerHostConfig{Mounts: []json.RawMessage{
			json.RawMessage(`{"Type":"volume","Source":"active-volume","Target":"/bin"}`),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing initial copy-up") {
		t.Fatalf("copy-up into live volume error = %v", err)
	}
	entries, err := os.ReadDir(item.Path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("live volume was modified: entries=%v err=%v", entries, err)
	}
}
