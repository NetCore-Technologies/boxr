package engineapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

func TestDockerConfigExposedPortsMergeImageAndCreateMetadata(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("fixture data\n"), 0644); err != nil {
		t.Fatal(err)
	}
	image, err := imageStore.ImportRootFSWithConfig(root, "registry.example/team/exposed-ports:v1", "linux", "amd64", "", imageStore.ImageRuntimeConfig{
		Cmd: []string{"/bin/true"},
		ExposedPorts: map[string]struct{}{
			"443/tcp": {},
			"53/udp":  {},
		},
	})
	if err != nil {
		t.Fatalf("import image with exposed ports: %v", err)
	}
	record, err := createContainerRecord("exposed-ports", dockerCreateRequest{
		Image: image.Reference,
		ExposedPorts: map[string]json.RawMessage{
			"80":       json.RawMessage("{}"),
			"8080/udp": json.RawMessage("{}"),
		},
	})
	if err != nil {
		t.Fatalf("create container with Config.ExposedPorts: %v", err)
	}
	t.Cleanup(func() {
		if err := state.RemoveCreated(record.ID); err != nil {
			t.Errorf("remove container: %v", err)
		}
	})
	want := []string{"443/tcp", "53/udp", "80/tcp", "8080/udp"}
	if !reflect.DeepEqual(record.Options.ExposedPorts, want) {
		t.Fatalf("container exposed ports = %#v, want %#v", record.Options.ExposedPorts, want)
	}
	imageConfig := dockerImageConfig(image.RuntimeConfig)
	imagePorts, ok := imageConfig["ExposedPorts"].(map[string]struct{})
	if !ok || len(imagePorts) != 2 {
		t.Fatalf("Docker image inspect ExposedPorts = %#v", imageConfig["ExposedPorts"])
	}
}

func TestDockerConfigExposedPortsRejectMalformedMetadata(t *testing.T) {
	for _, test := range []struct {
		name  string
		value map[string]json.RawMessage
	}{
		{name: "zero", value: map[string]json.RawMessage{"0/tcp": json.RawMessage("{}")}},
		{name: "too large", value: map[string]json.RawMessage{"65536/tcp": json.RawMessage("{}")}},
		{name: "unknown protocol", value: map[string]json.RawMessage{"80/icmp": json.RawMessage("{}")}},
		{name: "non-empty object", value: map[string]json.RawMessage{"80/tcp": json.RawMessage(`{"host":true}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := dockerConfigExposedPorts(test.value, nil); err == nil {
				t.Fatal("invalid exposed-port metadata was accepted")
			}
		})
	}
}
