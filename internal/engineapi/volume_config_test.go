package engineapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

func TestDockerConfigVolumesCreateAnonymousVolumesAndCopyImageData(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	image := importEngineAPITestImage(t, "registry.example/team/config-volumes:v1")

	record, err := createContainerRecord("config-volumes", dockerCreateRequest{
		Image: image.Reference,
		Volumes: map[string]json.RawMessage{
			"/bin":          json.RawMessage("{}"),
			"/srv/app/data": json.RawMessage("{}"),
		},
	})
	if err != nil {
		t.Fatalf("create container with Config.Volumes: %v", err)
	}
	t.Cleanup(func() {
		if err := state.RemoveCreated(record.ID); err != nil {
			t.Errorf("remove container: %v", err)
		}
		for _, mount := range record.Options.BindMounts {
			if mount.VolumeName != "" {
				if err := volume.Remove(mount.VolumeName); err != nil {
					t.Errorf("remove anonymous volume %s: %v", mount.VolumeName, err)
				}
			}
		}
	})

	if len(record.Options.BindMounts) != 2 {
		t.Fatalf("container bind mounts = %+v, want two declared volumes", record.Options.BindMounts)
	}
	var binVolume volume.Volume
	for _, mount := range record.Options.BindMounts {
		item, err := volume.Get(mount.VolumeName)
		if err != nil {
			t.Fatalf("get created volume %q: %v", mount.VolumeName, err)
		}
		if !item.Anonymous || mount.Source != item.Path || !mount.CopyUp {
			t.Fatalf("declared volume mount = %+v, volume = %+v", mount, item)
		}
		if mount.Target == "/bin" {
			binVolume = item
		}
	}
	if binVolume.Name == "" {
		t.Fatal("missing anonymous volume for /bin")
	}
	rootfs, err := state.ManagedRootFSDir(record.ID)
	if err != nil {
		t.Fatalf("get created container rootfs: %v", err)
	}
	mountpoint, err := os.Stat(filepath.Join(rootfs, "srv", "app", "data"))
	if err != nil || !mountpoint.IsDir() {
		t.Fatalf("declared volume mountpoint was not created in the rootfs: info=%v err=%v", mountpoint, err)
	}
	data, err := os.ReadFile(filepath.Join(binVolume.Path, "app"))
	if err != nil || string(data) != "boxr test image\n" {
		t.Fatalf("image data was not copied into declared volume: %q, %v", data, err)
	}
}

func TestDockerConfigVolumeExplicitMountOverridesAnonymousVolume(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	existing, err := volume.Create("explicit-volume", nil)
	if err != nil {
		t.Fatal(err)
	}
	image := importEngineAPITestImage(t, "registry.example/team/config-volume-override:v1")
	record, err := createContainerRecord("explicit-volume-override", dockerCreateRequest{
		Image: image.Reference,
		Volumes: map[string]json.RawMessage{
			"/bin": json.RawMessage("{}"),
		},
		HostConfig: dockerHostConfig{Binds: []string{"explicit-volume:/bin"}},
	})
	if err != nil {
		t.Fatalf("create container with explicit volume override: %v", err)
	}
	t.Cleanup(func() {
		if err := state.RemoveCreated(record.ID); err != nil {
			t.Errorf("remove container: %v", err)
		}
		if err := volume.Remove(existing.Name); err != nil {
			t.Errorf("remove explicit volume: %v", err)
		}
	})
	if len(record.Options.BindMounts) != 1 || record.Options.BindMounts[0].VolumeName != existing.Name {
		t.Fatalf("explicit mount was not preferred: %+v", record.Options.BindMounts)
	}
	items, err := volume.List()
	if err != nil || len(items) != 1 || items[0].Name != existing.Name {
		t.Fatalf("unexpected anonymous volume creation: %+v, %v", items, err)
	}
}

func TestDockerConfigVolumesRejectUnsafePathsAndMalformedValues(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	for _, test := range []struct {
		name   string
		target string
		value  json.RawMessage
	}{
		{name: "root", target: "/", value: json.RawMessage("{}")},
		{name: "relative", target: "data", value: json.RawMessage("{}")},
		{name: "traversal", target: "/data/../host", value: json.RawMessage("{}")},
		{name: "non-empty config", target: "/data", value: json.RawMessage(`{"driver":"host"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := importEngineAPITestImage(t, "registry.example/team/invalid-volume-"+test.name+":v1")
			_, err := createContainerRecord("invalid-volume-"+test.name, dockerCreateRequest{
				Image: image.Reference,
				Volumes: map[string]json.RawMessage{test.target: test.value},
			})
			if err == nil {
				t.Fatal("invalid Config.Volumes entry was accepted")
			}
			items, listErr := volume.List()
			if listErr != nil || len(items) != 0 {
				t.Fatalf("invalid create left volumes behind: %+v, %v", items, listErr)
			}
		})
	}
}

func TestImageConfigVolumesAppearInDockerImageConfigAndCreate(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "seed"), []byte("seed-data"), 0644); err != nil {
		t.Fatal(err)
	}
	image, err := imageStore.ImportRootFSWithConfig(root, "registry.example/team/image-volume-metadata:v1", "linux", "amd64", "", imageStore.ImageRuntimeConfig{
		Entrypoint: []string{"/bin/app"},
		Volumes:    map[string]struct{}{"/data": {}},
	})
	if err != nil {
		t.Fatalf("import image with volume metadata: %v", err)
	}
	config := dockerImageConfig(image.RuntimeConfig)
	declared, ok := config["Volumes"].(map[string]struct{})
	if !ok || len(declared) != 1 {
		t.Fatalf("Docker image config volumes = %#v", config["Volumes"])
	}
	record, err := createContainerRecord("image-volume-metadata", dockerCreateRequest{Image: image.Reference})
	if err != nil {
		t.Fatalf("create from image-declared volume: %v", err)
	}
	t.Cleanup(func() {
		if err := state.RemoveCreated(record.ID); err != nil {
			t.Errorf("remove container: %v", err)
		}
		for _, mount := range record.Options.BindMounts {
			if mount.VolumeName != "" {
				if err := volume.Remove(mount.VolumeName); err != nil {
					t.Errorf("remove image-declared anonymous volume: %v", err)
				}
			}
		}
	})
	if len(record.Options.BindMounts) != 1 || record.Options.BindMounts[0].Target != "/data" {
		t.Fatalf("image Config.Volumes was not mounted: %+v", record.Options.BindMounts)
	}
	contents, err := os.ReadFile(filepath.Join(record.Options.BindMounts[0].Source, "seed"))
	if err != nil || string(contents) != "seed-data" {
		t.Fatalf("image-declared volume copy-up = %q, %v", contents, err)
	}
}

func TestDockerConfigVolumesRollBackWhenContainerValidationFails(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	root := filepath.Join(t.TempDir(), "empty-rootfs")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	image, err := imageStore.ImportRootFSWithConfig(root, "registry.example/team/volume-rollback:v1", "linux", "amd64", "", imageStore.ImageRuntimeConfig{
		Volumes: map[string]struct{}{"/data": {}},
	})
	if err != nil {
		t.Fatalf("import image: %v", err)
	}
	if _, err := createContainerRecord("volume-rollback", dockerCreateRequest{Image: image.Reference}); err == nil {
		t.Fatal("container without an image command unexpectedly passed validation")
	}
	items, err := volume.List()
	if err != nil || len(items) != 0 {
		t.Fatalf("failed container create leaked anonymous volumes: %+v, %v", items, err)
	}
}

func TestDockerCreateEntrypointOverrideAndExplicitClear(t *testing.T) {
	setupEngineAPITest(t)
	image := importEngineAPITestImage(t, "registry.example/team/entrypoint-override:v1")
	tests := []struct {
		name       string
		entrypoint []string
		cmd        []string
		want       []string
	}{
		{
			name:       "replace image entrypoint",
			entrypoint: []string{"/usr/local/bin/custom"},
			cmd:        []string{"--custom"},
			want:       []string{"/usr/local/bin/custom", "--custom"},
		},
		{
			name:       "clear image entrypoint",
			entrypoint: []string{},
			want:       []string{"--serve"},
		},
		{
			name: "inherit image entrypoint",
			cmd:  []string{"--request"},
			want: []string{"/bin/app", "--request"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, err := createContainerRecord(test.name, dockerCreateRequest{
				Image: image.Reference, Entrypoint: test.entrypoint, Cmd: test.cmd,
			})
			if err != nil {
				t.Fatalf("create with Entrypoint=%v Cmd=%v: %v", test.entrypoint, test.cmd, err)
			}
			t.Cleanup(func() {
				if err := state.RemoveCreated(record.ID); err != nil {
					t.Errorf("remove container: %v", err)
				}
			})
			if len(record.Command) != len(test.want) {
				t.Fatalf("container command = %v, want %v", record.Command, test.want)
			}
			for index := range test.want {
				if record.Command[index] != test.want[index] {
					t.Fatalf("container command = %v, want %v", record.Command, test.want)
				}
			}
		})
	}
}
