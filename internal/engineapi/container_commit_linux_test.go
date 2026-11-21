//go:build linux

package engineapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

func createDockerCommitTestContainer(t *testing.T, server *Server, imageReference string) state.Record {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"Image": imageReference,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/create", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("container create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	record, err := state.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestDockerContainerCommitSnapshotsWritableLayerAndConfig(t *testing.T) {
	server := setupEngineAPITest(t)
	importEngineAPITestImage(t, "commit-base:latest")
	record := createDockerCommitTestContainer(t, server, "commit-base:latest")

	upper := filepath.Join(record.Options.OverlayDir, "upper")
	if err := os.MkdirAll(filepath.Join(upper, "etc", "boxr"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "etc", "boxr", "committed.conf"), []byte("committed=true" + string([]byte{10})), 0640); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"Cmd":["--committed"],"Labels":{"org.example.commit":"true"}}`)
	request := httptest.NewRequest(http.MethodPost,
		"/v1.45/commit?container="+record.ID+"&repo=committed-app&tag=test", body)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("commit status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID == "" {
		t.Fatal("commit response omitted image ID")
	}

	image, err := imageStore.Inspect("committed-app:test")
	if err != nil {
		t.Fatal(err)
	}
	if len(image.RuntimeConfig.Cmd) != 1 || image.RuntimeConfig.Cmd[0] != "--committed" {
		t.Fatalf("committed image Cmd = %#v", image.RuntimeConfig.Cmd)
	}
	if image.RuntimeConfig.Labels["org.example.commit"] != "true" {
		t.Fatalf("committed image labels = %#v", image.RuntimeConfig.Labels)
	}
	unpacked := filepath.Join(t.TempDir(), "unpacked")
	if err := imageStore.Unpack(image.Reference, unpacked); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(unpacked, "etc", "boxr", "committed.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "committed=true" + string([]byte{10}) {
		t.Fatalf("committed writable-layer content = %q", data)
	}
}

func TestDockerContainerCommitExcludesDeclaredVolumeContents(t *testing.T) {
	server := setupEngineAPITest(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "app"), []byte("test image"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "from-image.txt"), []byte("volume content"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := imageStore.ImportRootFSWithConfig(root, "volume-commit-base:latest", "linux", "amd64", "",
		imageStore.ImageRuntimeConfig{
			Entrypoint: []string{"/bin/app"},
			Volumes: map[string]struct{}{"/data": {}},
		})
	if err != nil {
		t.Fatal(err)
	}
	record := createDockerCommitTestContainer(t, server, "volume-commit-base:latest")
	if len(record.Options.BindMounts) == 0 {
		t.Fatal("declared image volume was not mounted into the test container")
	}

	response := engineAPIRequest(server, http.MethodPost,
		"/commit?container="+record.ID+"&repo=volume-commit-result&tag=latest")
	if response.Code != http.StatusCreated {
		t.Fatalf("commit status=%d body=%s", response.Code, response.Body.String())
	}
	unpacked := filepath.Join(t.TempDir(), "volume-committed")
	if err := imageStore.Unpack("volume-commit-result:latest", unpacked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "data", "from-image.txt")); !os.IsNotExist(err) {
		t.Fatalf("committed image included mounted volume data: %v", err)
	}
	info, err := os.Stat(filepath.Join(unpacked, "data"))
	if err != nil || !info.IsDir() {
		t.Fatalf("committed image did not preserve an empty volume mountpoint: %v", err)
	}
}

func TestDockerContainerCommitRejectsRunningContainersAndUnsafeTargets(t *testing.T) {
	server := setupEngineAPITest(t)
	importEngineAPITestImage(t, "commit-running-base:latest")
	record := createDockerCommitTestContainer(t, server, "commit-running-base:latest")
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodPost, "/commit?container="+record.ID)
	if response.Code != http.StatusConflict {
		t.Fatalf("running container commit status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := dockerCommitExcludedMounts(record, imageStore.ImageRuntimeConfig{
		Volumes: map[string]struct{}{"/../host": {}},
	}); err == nil {
		t.Fatal("commit accepted a mount target with parent traversal")
	}
	if _, err := dockerCommitReference(record.ID, "", "latest"); err == nil {
		t.Fatal("commit accepted a tag without a repository")
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("must be stopped")) {
		t.Fatalf("running-container rejection = %s", response.Body.String())
	}
}
