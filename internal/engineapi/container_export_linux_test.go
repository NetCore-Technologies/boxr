//go:build linux

package engineapi

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
)

func TestDockerContainerExportReturnsMergedFilesystemTar(t *testing.T) {
	server := setupEngineAPITest(t)
	importEngineAPITestImage(t, "export-base:latest")
	record := createDockerCommitTestContainer(t, server, "export-base:latest")
	upper := filepath.Join(record.Options.OverlayDir, "upper")
	if err := os.MkdirAll(filepath.Join(upper, "var", "lib", "boxr"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "var", "lib", "boxr", "exported.txt"), []byte("from-upper"), 0600); err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodGet, "/v1.45/containers/"+record.ID+"/export")
	if response.Code != http.StatusOK {
		t.Fatalf("container export status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/x-tar" {
		t.Fatalf("container export Content-Type=%q", got)
	}
	reader := tar.NewReader(bytes.NewReader(response.Body.Bytes()))
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "var/lib/boxr/exported.txt" {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "from-upper" {
				t.Fatalf("exported writable-layer file = %q", data)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("container export omitted a writable-layer file")
	}
}

func TestDockerContainerExportExcludesMountedVolumeContents(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(root, "data", "mounted.txt"), []byte("must not export"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := imageStore.ImportRootFSWithConfig(root, "export-volume-base:latest", "linux", "amd64", "",
		imageStore.ImageRuntimeConfig{
			Entrypoint: []string{"/bin/app"},
			Volumes: map[string]struct{}{"/data": {}},
		})
	if err != nil {
		t.Fatal(err)
	}
	record := createDockerCommitTestContainer(t, server, "export-volume-base:latest")
	if len(record.Options.BindMounts) == 0 {
		t.Fatal("declared volume was not mounted into the test container")
	}
	response := engineAPIRequest(server, http.MethodGet, "/containers/"+record.ID+"/export")
	if response.Code != http.StatusOK {
		t.Fatalf("container export status=%d body=%s", response.Code, response.Body.String())
	}
	reader := tar.NewReader(bytes.NewReader(response.Body.Bytes()))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "data/mounted.txt" {
			t.Fatal("container export included mounted volume contents")
		}
	}
}

func TestDockerContainerExportRejectsInvalidRequests(t *testing.T) {
	server := setupEngineAPITest(t)
	importEngineAPITestImage(t, "export-validation-base:latest")
	record := createDockerCommitTestContainer(t, server, "export-validation-base:latest")
	method := engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/export")
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST container export status=%d body=%s", method.Code, method.Body.String())
	}
	query := engineAPIRequest(server, http.MethodGet, "/containers/"+record.ID+"/export?unexpected=1")
	if query.Code != http.StatusBadRequest {
		t.Fatalf("container export query status=%d body=%s", query.Code, query.Body.String())
	}
}
