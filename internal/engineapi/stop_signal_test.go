package engineapi

import (
	"os"
	"path/filepath"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

func TestDockerCreateStopSignalOverridesAndInheritsImageDefault(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("stop signal fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	image, err := imageStore.ImportRootFSWithConfig(root, "registry.example/team/stopsignal:v1", "linux", "amd64", "", imageStore.ImageRuntimeConfig{
		Cmd:        []string{"/bin/true"},
		StopSignal: "SIGQUIT",
	})
	if err != nil {
		t.Fatalf("import image with STOPSIGNAL: %v", err)
	}
	imageConfig := dockerImageConfig(image.RuntimeConfig)
	if imageConfig["StopSignal"] != "SIGQUIT" {
		t.Fatalf("image inspect StopSignal = %#v", imageConfig["StopSignal"])
	}

	for _, test := range []struct {
		name     string
		override string
		want     string
	}{
		{name: "inherits image default", want: "SIGQUIT"},
		{name: "overrides image default", override: "SIGINT", want: "SIGINT"},
		{name: "numeric signal", override: "2", want: "SIGINT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := createContainerRecord(test.name, dockerCreateRequest{
				Image: image.Reference, StopSignal: test.override,
			})
			if err != nil {
				t.Fatalf("create container: %v", err)
			}
			t.Cleanup(func() {
				if err := state.RemoveCreated(record.ID); err != nil {
					t.Errorf("remove container: %v", err)
				}
			})
			if record.Options.StopSignal != test.want {
				t.Fatalf("persisted StopSignal = %q, want %q", record.Options.StopSignal, test.want)
			}
			got, err := dockerConfigStopSignal(record.Options.StopSignal, "")
			if err != nil || got != test.want {
				t.Fatalf("inspect StopSignal = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestDockerCreateRejectsUnsupportedStopSignal(t *testing.T) {
	for _, value := range []string{"SIGFOO", "0", "11", "TERM KILL"} {
		if _, err := dockerConfigStopSignal(value, ""); err == nil {
			t.Errorf("unsupported StopSignal %q was accepted", value)
		}
	}
}
