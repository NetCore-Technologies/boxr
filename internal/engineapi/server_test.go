package engineapi

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

func setupEngineAPITest(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BOXR_STATE_DIR", root+"/state")
	t.Setenv("BOXR_IMAGE_DIR", root+"/images")
	return New("test-version")
}

func engineAPIRequest(server *Server, method, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func createEngineAPITestContainer(t *testing.T, name string, options container.Options) state.Record {
	t.Helper()
	record, err := state.Create(name, []string{"/usr/bin/example", "--serve"}, options, map[string]string{"team": "platform"})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	return record
}

func importEngineAPITestImage(t *testing.T, reference string) imageStore.Image {
	t.Helper()
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "app"), []byte("boxr test image\n"), 0755); err != nil {
		t.Fatal(err)
	}
	image, err := imageStore.ImportRootFSWithConfig(root, reference, "linux", "amd64", "", imageStore.ImageRuntimeConfig{
		Env: []string{"APP_MODE=test"}, Entrypoint: []string{"/bin/app"},
		Cmd: []string{"--serve"}, WorkingDir: "/srv/app",
		Labels: map[string]string{"org.example.component": "api"},
	})
	if err != nil {
		t.Fatalf("import test image: %v", err)
	}
	return image
}

func TestPingAndVersionSupportDockerAPIVersionPrefix(t *testing.T) {
	server := setupEngineAPITest(t)
	for _, target := range []string{"/_ping", "/v1.45/_ping"} {
		response := engineAPIRequest(server, http.MethodGet, target)
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "OK" {
			t.Fatalf("%s: status=%d body=%q", target, response.Code, response.Body.String())
		}
		if response.Header().Get("API-Version") != apiVersion {
			t.Fatalf("%s: API-Version=%q", target, response.Header().Get("API-Version"))
		}
	}
	response := engineAPIRequest(server, http.MethodGet, "/v1.45/version")
	if response.Code != http.StatusOK {
		t.Fatalf("version status=%d body=%s", response.Code, response.Body.String())
	}
	var version map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &version); err != nil {
		t.Fatal(err)
	}
	if version["ApiVersion"] != apiVersion || version["MinAPIVersion"] != minAPIVersion || version["Version"] != "test-version" {
		t.Fatalf("Docker version response = %#v", version)
	}
}

func TestVersionNegotiationRejectsUnsupportedAPIRanges(t *testing.T) {
	server := setupEngineAPITest(t)
	for _, target := range []string{"/v2.0/info", "/v1.23/info", "/v1.46/info"} {
		response := engineAPIRequest(server, http.MethodGet, target)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", target, response.Code)
		}
	}
}

func TestContainerListInspectFiltersAndDockerFields(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "api-test", container.Options{
		NetworkMode: "bridge", NetworkName: "boxr-test",
		Hostname: "api", Workdir: "/srv/app", MemoryLimitBytes: 64 << 20,
		PublishedPorts: []container.PortMapping{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		BindMounts:     []container.BindMount{{Source: "/srv/data", Target: "/data", ReadOnly: true}},
	})
	filters, err := json.Marshal(map[string][]string{"name": {"api-test"}, "status": {"running"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(filters)
	response := engineAPIRequest(server, http.MethodGet, "/v1.45/containers/json?filters="+url.QueryEscape(encoded))
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["Id"] != record.ID || items[0]["State"] != "running" {
		t.Fatalf("container list = %#v", items)
	}
	ports, ok := items[0]["Ports"].([]any)
	if !ok || len(ports) != 1 {
		t.Fatalf("container list ports = %#v", items[0]["Ports"])
	}

	response = engineAPIRequest(server, http.MethodGet, "/v1.45/containers/"+record.ID+"/json")
	if response.Code != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", response.Code, response.Body.String())
	}
	var inspect map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &inspect); err != nil {
		t.Fatal(err)
	}
	config := inspect["Config"].(map[string]any)
	hostConfig := inspect["HostConfig"].(map[string]any)
	network := inspect["NetworkSettings"].(map[string]any)
	mounts := inspect["Mounts"].([]any)
	if inspect["Id"] != record.ID || inspect["Name"] != "/api-test" || config["Hostname"] != "api" {
		t.Fatalf("container inspect = %#v", inspect)
	}
	if hostConfig["NetworkMode"] != "boxr-test" || hostConfig["Memory"] != float64(64<<20) {
		t.Fatalf("HostConfig = %#v", hostConfig)
	}
	if len(mounts) != 1 || network["Ports"] == nil {
		t.Fatalf("mounts or ports missing: mounts=%#v networks=%#v", mounts, network)
	}
}

func TestContainerLogsUseDockerMultiplexedFrames(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("log-test", []string{"/bin/echo", "hello"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello from Boxr\n")
	if _, err := logFile.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodGet, "/containers/"+record.ID+"/logs?stdout=1&stderr=1")
	if response.Code != http.StatusOK {
		t.Fatalf("logs status=%d body=%q", response.Code, response.Body.String())
	}
	body := response.Body.Bytes()
	if len(body) != len(payload)+8 || body[0] != 1 || body[1] != 0 || body[2] != 0 || body[3] != 0 {
		t.Fatalf("unexpected Docker log frame: %v", body)
	}
	if got := binary.BigEndian.Uint32(body[4:8]); got != uint32(len(payload)) {
		t.Fatalf("frame length=%d want %d", got, len(payload))
	}
	if string(body[8:]) != string(payload) {
		t.Fatalf("frame payload=%q", body[8:])
	}
}

func TestEventsStreamDockerContainerEventShape(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "event-test", container.Options{})
	until := time.Now().Add(time.Minute).Unix()
	response := engineAPIRequest(server, http.MethodGet, "/v1.45/events?until="+strconv.FormatInt(until, 10))
	if response.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", response.Code, response.Body.String())
	}
	scanner := bufio.NewScanner(strings.NewReader(response.Body.String()))
	found := false
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		actor := event["Actor"].(map[string]any)
		if event["Type"] == "container" && actor["ID"] == record.ID {
			found = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("events did not include container %s: %s", record.ID, response.Body.String())
	}
}

func newEngineAPITestRegistry(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	fileData := []byte("pulled OCI image\n")
	var tarBytes bytes.Buffer
	tarWriter := tar.NewWriter(&tarBytes)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "bin/app", Typeflag: tar.TypeReg, Mode: 0755, Size: int64(len(fileData))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(fileData); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(tarBytes.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	layerDigestBytes := sha256.Sum256(compressed.Bytes())
	layerDigest := "sha256:" + hex.EncodeToString(layerDigestBytes[:])
	diffIDBytes := sha256.Sum256(tarBytes.Bytes())
	diffID := "sha256:" + hex.EncodeToString(diffIDBytes[:])
	configBytes, err := json.Marshal(map[string]any{
		"os": "linux", "architecture": "amd64",
		"config": map[string]any{
			"Env": []string{"PULLED=1"}, "Entrypoint": []string{"/bin/app"},
			"Cmd": []string{"--serve"}, "WorkingDir": "/srv/pulled",
			"Labels": map[string]string{"org.example.pull": "verified"},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{diffID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	configDigestBytes := sha256.Sum256(configBytes)
	configDigest := "sha256:" + hex.EncodeToString(configDigestBytes[:])
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest, "size": len(configBytes),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    layerDigest, "size": compressed.Len(),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigestBytes := sha256.Sum256(manifest)
	manifestDigest := "sha256:" + hex.EncodeToString(manifestDigestBytes[:])
	username, password := "boxr-test", "registry-secret"

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)) {
			writer.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q", "boxr-test"))
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/v2/team/app/manifests/v1":
			writer.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			writer.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = writer.Write(manifest)
		case "/v2/team/app/blobs/" + configDigest:
			writer.Header().Set("Content-Type", "application/octet-stream")
			_, _ = writer.Write(configBytes)
		case "/v2/team/app/blobs/" + layerDigest:
			writer.Header().Set("Content-Type", "application/octet-stream")
			_, _ = writer.Write(compressed.Bytes())
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	registryHost := strings.TrimPrefix(server.URL, "http://")
	return server, registryHost + "/team/app"
}

func newEngineAPITestPushRegistry(t *testing.T) (*httptest.Server, string, func() (int, bool)) {
	t.Helper()
	username, password := "boxr-push", "registry-push-secret"
	uploaded := make(map[string][]byte)
	var publishedManifest []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
		if request.Header.Get("Authorization") != expectedAuth {
			writer.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q", "boxr-push"))
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v2/team/app/blobs/uploads/":
			writer.Header().Set("Location", "/v2/team/app/blobs/uploads/session?upload=1")
			writer.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodPut && request.URL.Path == "/v2/team/app/blobs/uploads/session":
			digest := request.URL.Query().Get("digest")
			data, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read uploaded blob: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			sum := sha256.Sum256(data)
			actualDigest := "sha256:" + hex.EncodeToString(sum[:])
			if digest != actualDigest {
				t.Errorf("uploaded blob digest=%q, want %q", digest, actualDigest)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			uploaded[digest] = append([]byte(nil), data...)
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusCreated)
		case request.Method == http.MethodPut && request.URL.Path == "/v2/team/app/manifests/v1":
			data, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read published manifest: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			var manifest map[string]any
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Errorf("decode published manifest: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			layers, _ := manifest["layers"].([]any)
			if len(layers) != 1 {
				t.Errorf("published manifest layers=%d, want one", len(layers))
			} else if layer, ok := layers[0].(map[string]any); !ok || layer["mediaType"] != "application/vnd.oci.image.layer.v1.tar+gzip" {
				t.Errorf("published layer descriptor=%#v", layers[0])
			}
			publishedManifest = append([]byte(nil), data...)
			sum := sha256.Sum256(data)
			writer.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	check := func() (int, bool) { return len(uploaded), len(publishedManifest) > 0 }
	return server, host, check
}

func TestEngineAPIImageTagCreatesDockerCompatibleAliases(t *testing.T) {
	server := setupEngineAPITest(t)
	const source = "registry.example/team/boxr-tag:v1"
	importEngineAPITestImage(t, source)
	response := engineAPIRequest(server, http.MethodPost, "/images/"+source+"/tag?repo=registry.example/team/boxr-tag&tag=stable")
	if response.Code != http.StatusCreated {
		t.Fatalf("tag status=%d body=%s", response.Code, response.Body.String())
	}
	alias, err := imageStore.Inspect("registry.example/team/boxr-tag:stable")
	if err != nil {
		t.Fatalf("tagged image not found: %v", err)
	}
	original, err := imageStore.Inspect(source)
	if err != nil {
		t.Fatal(err)
	}
	if alias.Digest != original.Digest || alias.DiffIDs[0] != original.DiffIDs[0] {
		t.Fatalf("tagged image does not refer to source content: alias=%+v original=%+v", alias, original)
	}
	response = engineAPIRequest(server, http.MethodPost, "/images/"+source+"/tag")
	if response.Code != http.StatusCreated {
		t.Fatalf("default tag status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := imageStore.Inspect("registry.example/team/boxr-tag:latest"); err != nil {
		t.Fatalf("default latest alias not found: %v", err)
	}
}

func TestEngineAPIImageTagRejectsInvalidReferencesAndInUseTargets(t *testing.T) {
	server := setupEngineAPITest(t)
	response := engineAPIRequest(server, http.MethodPost, "/images/registry.example/team/missing:v1/tag?repo=registry.example/team/missing&tag=alias")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing source status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/images/registry.example/team/boxr%20invalid:v1/tag")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid source reference status=%d body=%s", response.Code, response.Body.String())
	}

	const source = "registry.example/team/boxr-in-use:v1"
	importEngineAPITestImage(t, source)
	const target = "registry.example/team/boxr-in-use:stable"
	record, err := state.Create("tag-target-user", []string{"/bin/app"}, container.Options{}, map[string]string{"io.boxr.image": target})
	if err != nil {
		t.Fatal(err)
	}
	defer state.RemoveCreated(record.ID)
	response = engineAPIRequest(server, http.MethodPost, "/images/"+source+"/tag?repo=registry.example/team/boxr-in-use&tag=stable")
	if response.Code != http.StatusConflict {
		t.Fatalf("retag in-use target status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := imageStore.Inspect(target); err == nil {
		t.Fatal("in-use target tag should not have been created or overwritten")
	}
}

func TestEngineAPIImagePushPublishesVerifiedOCIImageWithDockerAuth(t *testing.T) {
	server := setupEngineAPITest(t)
	registryServer, registryHost, check := newEngineAPITestPushRegistry(t)
	reference := registryHost + "/team/app:v1"
	importEngineAPITestImage(t, reference)

	authConfig, err := json.Marshal(map[string]string{
		"username": "boxr-push", "password": "registry-push-secret",
		"serveraddress": registryServer.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/v1.45/images/"+registryHost+"/team/app/push?tag=v1", nil)
	request.Header.Set("X-Registry-Auth", base64.RawURLEncoding.EncodeToString(authConfig))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("image push status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Push complete") {
		t.Fatalf("image push progress stream = %s", response.Body.String())
	}
	uploadedCount, manifestPublished := check()
	if uploadedCount != 2 || !manifestPublished {
		t.Fatalf("registry received %d blobs and manifestPublished=%t; want 2 blobs and a manifest", uploadedCount, manifestPublished)
	}
}

func TestEngineAPIImagePushRejectsMissingImagesAndMalformedQueries(t *testing.T) {
	server := setupEngineAPITest(t)
	response := engineAPIRequest(server, http.MethodPost, "/images/registry.example/team/app/push")
	if response.Code != http.StatusNotFound {
		t.Fatalf("push missing image status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/images/registry.example/team/app/push?tag=")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("push empty tag status=%d body=%s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/images/registry.example/team/app/push", nil)
	request.Header.Set("X-Registry-Auth", "not-base64!")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("push invalid registry auth status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEngineAPIImageCreatePullsAuthenticatedOCIImage(t *testing.T) {
	server := setupEngineAPITest(t)
	registryServer, remoteImage := newEngineAPITestRegistry(t)
	localReference := remoteImage + ":v1"
	authConfig, err := json.Marshal(map[string]string{
		"username": "boxr-test", "password": "registry-secret",
		"serveraddress": registryServer.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	authHeader := base64.RawURLEncoding.EncodeToString(authConfig)
	target := "/v1.45/images/create?fromImage=" + url.QueryEscape(remoteImage) + "&tag=v1&platform=linux%2Famd64"
	request := httptest.NewRequest(http.MethodPost, target, nil)
	request.Header.Set("X-Registry-Auth", authHeader)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("image pull status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Pull complete") || !strings.Contains(response.Body.String(), "Digest: sha256:") {
		t.Fatalf("image pull progress stream = %s", response.Body.String())
	}
	image, err := imageStore.Inspect(localReference)
	if err != nil {
		t.Fatalf("pulled image was not imported: %v", err)
	}
	if image.RuntimeConfig.WorkingDir != "/srv/pulled" || len(image.RuntimeConfig.Env) != 1 || image.RuntimeConfig.Env[0] != "PULLED=1" {
		t.Fatalf("pulled runtime config = %+v", image.RuntimeConfig)
	}
	root := filepath.Join(t.TempDir(), "unpacked")
	if _, err := imageStore.Unpack(localReference, root); err != nil {
		t.Fatalf("unpack pulled image: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "bin", "app")); err != nil || string(data) != "pulled OCI image\n" {
		t.Fatalf("pulled image file = %q, error=%v", data, err)
	}
	defaultTag := remoteImage + ":latest"
	if err := imageStore.Tag(localReference, defaultTag); err != nil {
		t.Fatalf("tag pulled image as latest: %v", err)
	}
	created, err := createContainerRecord("pulled-container", dockerCreateRequest{Image: remoteImage})
	if err != nil {
		t.Fatalf("create container from pulled image reference: %v", err)
	}
	if created.Labels["io.boxr.image"] != defaultTag || strings.Join(created.Command, " ") != "/bin/app --serve" {
		t.Fatalf("container created from untagged pulled image = %+v", created)
	}
	if err := state.RemoveCreated(created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEngineAPIImageCreateRejectsInvalidQueriesAndAuth(t *testing.T) {
	server := setupEngineAPITest(t)
	for _, target := range []string{
		"/images/create",
		"/images/create?fromImage=not-a-valid-reference%20value",
		"/images/create?fromImage=example.com/team/app&tag=",
	} {
		response := engineAPIRequest(server, http.MethodPost, target)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/images/create?fromImage=example.com/team/app:v1", nil)
	request.Header.Set("X-Registry-Auth", "not-base64!")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid registry auth status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEngineAPIImageInspectReturnsVerifiedDockerMetadata(t *testing.T) {
	server := setupEngineAPITest(t)
	const reference = "registry.example/team/boxr-api:v1"
	created := importEngineAPITestImage(t, reference)

	response := engineAPIRequest(server, http.MethodGet, "/images/"+reference+"/json")
	if response.Code != http.StatusOK {
		t.Fatalf("image inspect status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["Id"] != dockerImageID(created.Digest) || body["Created"] == "" || body["Architecture"] != "amd64" || body["Os"] != "linux" {
		t.Fatalf("image inspect metadata = %#v", body)
	}
	if tags, ok := body["RepoTags"].([]any); !ok || len(tags) != 1 || tags[0] != reference {
		t.Fatalf("RepoTags = %#v", body["RepoTags"])
	}
	config, ok := body["Config"].(map[string]any)
	if !ok || config["WorkingDir"] != "/srv/app" || config["User"] != "" {
		t.Fatalf("Config = %#v", body["Config"])
	}
	if env, ok := config["Env"].([]any); !ok || len(env) != 1 || env[0] != "APP_MODE=test" {
		t.Fatalf("Config.Env = %#v", config["Env"])
	}
	rootFS, ok := body["RootFS"].(map[string]any)
	if !ok || rootFS["Type"] != "layers" {
		t.Fatalf("RootFS = %#v", body["RootFS"])
	}
	layers, ok := rootFS["Layers"].([]any)
	if !ok || len(layers) != 1 || layers[0] != created.DiffIDs[0] {
		t.Fatalf("RootFS.Layers = %#v, want diff ID %v", rootFS["Layers"], created.DiffIDs)
	}
	if driver, ok := body["GraphDriver"].(map[string]any); !ok || driver["Name"] != "boxr-oci" {
		t.Fatalf("GraphDriver = %#v", body["GraphDriver"])
	}
}

func TestEngineAPIImageDeleteRejectsInUseImagesAndUntagsUnusedImages(t *testing.T) {
	server := setupEngineAPITest(t)
	const reference = "registry.example/team/boxr-delete:v1"
	importEngineAPITestImage(t, reference)
	record, err := state.Create("image-user", []string{"/bin/app"}, container.Options{}, map[string]string{"io.boxr.image": reference})
	if err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodDelete, "/images/"+reference)
	if response.Code != http.StatusConflict {
		t.Fatalf("delete in-use image status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := imageStore.Inspect(reference); err != nil {
		t.Fatalf("in-use image was removed: %v", err)
	}
	response = engineAPIRequest(server, http.MethodDelete, "/images/"+reference+"?force=true")
	if response.Code != http.StatusConflict {
		t.Fatalf("forced delete in-use image status=%d body=%s", response.Code, response.Body.String())
	}

	if err := state.RemoveCreated(record.ID); err != nil {
		t.Fatal(err)
	}
	response = engineAPIRequest(server, http.MethodDelete, "/images/"+reference)
	if response.Code != http.StatusOK {
		t.Fatalf("delete unused image status=%d body=%s", response.Code, response.Body.String())
	}
	var result []map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0]["Untagged"] != reference {
		t.Fatalf("delete response = %#v", result)
	}
	if _, err := imageStore.Inspect(reference); err == nil {
		t.Fatal("image tag remained after delete")
	}
}

func TestEngineAPIImageRoutesValidateReferencesMethodsAndQuery(t *testing.T) {
	server := setupEngineAPITest(t)
	response := engineAPIRequest(server, http.MethodGet, "/images/not%20a%20valid/json")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid image inspect reference status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/images/demo:latest/json")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("image inspect wrong method status=%d", response.Code)
	}
	response = engineAPIRequest(server, http.MethodDelete, "/images/demo:latest?force=not-a-bool")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("image delete invalid force status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodGet, "/images/missing:latest/json")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing image inspect status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodDelete, "/images/missing:latest")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing image delete status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInfoImagesAndUnsupportedMethods(t *testing.T) {
	server := setupEngineAPITest(t)
	createEngineAPITestContainer(t, "info-test", container.Options{})
	info := engineAPIRequest(server, http.MethodGet, "/info")
	if info.Code != http.StatusOK {
		t.Fatalf("info status=%d body=%s", info.Code, info.Body.String())
	}
	var infoBody map[string]any
	if err := json.Unmarshal(info.Body.Bytes(), &infoBody); err != nil {
		t.Fatal(err)
	}
	if infoBody["Containers"] != float64(1) || infoBody["ContainersRunning"] != float64(1) || infoBody["OSType"] != "linux" {
		t.Fatalf("info response = %#v", infoBody)
	}
	images := engineAPIRequest(server, http.MethodGet, "/images/json")
	if images.Code != http.StatusOK || !strings.Contains(images.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("images status=%d content-type=%q", images.Code, images.Header().Get("Content-Type"))
	}
	if body := strings.TrimSpace(images.Body.String()); body != "[]" {
		t.Fatalf("empty image list = %q, want []", body)
	}
	unsupported := engineAPIRequest(server, http.MethodPost, "/containers/json")
	if unsupported.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported mutation status=%d", unsupported.Code)
	}
}

func TestMalformedContainerQueriesReturnDockerErrors(t *testing.T) {
	server := setupEngineAPITest(t)
	response := engineAPIRequest(server, http.MethodGet, "/containers/json?all=not-a-bool")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid boolean status=%d", response.Code)
	}
	response = engineAPIRequest(server, http.MethodGet, "/containers/json?filters=not-base64")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid filters status=%d", response.Code)
	}
	response = engineAPIRequest(server, http.MethodGet, "/containers/missing/json")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing container status=%d", response.Code)
	}
}

func TestDockerImageIDDoesNotDuplicateDigestAlgorithm(t *testing.T) {
	if got := dockerImageID("sha256:abc123"); got != "sha256:abc123" {
		t.Fatalf("prefixed image ID = %q", got)
	}
	if got := dockerImageID("abc123"); got != "sha256:abc123" {
		t.Fatalf("unprefixed image ID = %q", got)
	}
}

func TestEngineAPIValidatesLogAndEventQueryParameters(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("query-test", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodGet, "/containers/"+record.ID+"/logs?stdout=1&follow=not-a-bool")
	if response.Code != http.StatusBadRequest {
		t.Errorf("invalid log follow status=%d", response.Code)
	}
	response = engineAPIRequest(server, http.MethodGet, "/events?since=not-a-timestamp")
	if response.Code != http.StatusBadRequest {
		t.Errorf("invalid event timestamp status=%d", response.Code)
	}
	response = engineAPIRequest(server, http.MethodGet, "/events?until="+strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)+"&filters=not-base64")
	if response.Code != http.StatusBadRequest {
		t.Errorf("invalid event filters status=%d", response.Code)
	}
}

func TestEventFiltersMatchContainerActionsAndIDs(t *testing.T) {
	event := state.Event{Action: "start", ID: "abcdef123456", Name: "api"}
	tests := []struct {
		name    string
		filters map[string][]string
		want    bool
	}{
		{name: "type and event", filters: map[string][]string{"type": {"container"}, "event": {"start"}}, want: true},
		{name: "container ID prefix", filters: map[string][]string{"container": {"abcdef"}}, want: true},
		{name: "container name", filters: map[string][]string{"container": {"api"}}, want: true},
		{name: "wrong event", filters: map[string][]string{"event": {"die"}}, want: false},
		{name: "unsupported filter", filters: map[string][]string{"image": {"alpine"}}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesEventFilters(event, test.filters); got != test.want {
				t.Fatalf("matchesEventFilters() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestEngineAPIContainerKillSendsVerifiedDockerSignal(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("kill-test", []string{"sleep", "30"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sh", "-c", "exec sleep 30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	if err := state.Start(record.ID, child.Process.Pid); err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/kill?signal=15")
	if response.Code != http.StatusNoContent {
		t.Fatalf("kill status=%d body=%s", response.Code, response.Body.String())
	}
	if err := child.Wait(); err != nil {
		waited = true
		if finishErr := state.Finish(record.ID, err); finishErr != nil {
			t.Fatal(finishErr)
		}
	} else {
		waited = true
		t.Fatal("SIGTERM unexpectedly produced a successful process exit")
	}
}

func TestEngineAPIContainerKillValidatesSignalAndState(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("created-kill-test", []string{"sleep", "30"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/kill?signal=999")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid signal status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/kill")
	if response.Code != http.StatusConflict {
		t.Fatalf("created container kill status=%d", response.Code)
	}
	response = engineAPIRequest(server, http.MethodPost, "/containers/missing/kill")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing container kill status=%d", response.Code)
	}
}

func TestEngineAPIContainerDeleteCreatedAndRejectsRunning(t *testing.T) {
	server := setupEngineAPITest(t)
	created, err := state.Create("created-delete-test", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodDelete, "/containers/"+created.ID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete created status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := state.Get(created.ID); err == nil {
		t.Fatal("created container still exists after delete")
	}

	running := createEngineAPITestContainer(t, "running-delete-test", container.Options{})
	response = engineAPIRequest(server, http.MethodDelete, "/containers/"+running.ID)
	if response.Code != http.StatusConflict {
		t.Fatalf("delete running status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodDelete, "/containers/"+running.ID+"?force=not-a-bool")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid force query status=%d", response.Code)
	}
	if err := state.Finish(running.ID, nil); err != nil {
		t.Fatal(err)
	}
	response = engineAPIRequest(server, http.MethodDelete, "/containers/"+running.ID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete stopped status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEngineAPIContainerDeleteRejectsUnsupportedForceAndVolumeRemoval(t *testing.T) {
	server := setupEngineAPITest(t)
	created, err := state.Create("delete-options-test", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodDelete, "/containers/"+created.ID+"?v=true")
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("volume removal status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodDelete, "/containers/"+created.ID+"?force=true")
	if response.Code != http.StatusNoContent {
		t.Fatalf("force flag on a stopped created container status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerCreateRejectsUnsupportedAndMalformedRequests(t *testing.T) {
	server := setupEngineAPITest(t)
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "missing image", body: `{"Cmd":["/bin/sh"]}`, want: http.StatusBadRequest},
		{name: "TTY unsupported", body: `{"Image":"alpine:latest","Tty":true}`, want: http.StatusBadRequest},
		{name: "privileged unsupported", body: `{"Image":"alpine:latest","HostConfig":{"Privileged":true}}`, want: http.StatusBadRequest},
		{name: "unknown field rejected", body: `{"Image":"alpine:latest","HostConfig":{"Mystery":true}}`, want: http.StatusBadRequest},
		{name: "multiple JSON values rejected", body: `{"Image":"alpine:latest"} {}`, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/create", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("create status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
		})
	}
}

func TestDockerOptionsMapsResourcesPortsAndSafeNetworkDefaults(t *testing.T) {
	options, err := dockerOptions(dockerHostConfig{
		Memory: 256 << 20, MemoryReservation: 64 << 20, MemorySwap: 512 << 20,
		NanoCPUs: 1500000000, PidsLimit: 128, ReadonlyRootfs: true,
		CapDrop: []string{"NET_RAW"},
		PortBindings: map[string][]dockerPortBinding{
			"8080/tcp": {{HostIP: "127.0.0.1", HostPort: "18080"}},
			"5353/udp": {{HostIP: "127.0.0.1", HostPort: "15353"}},
		},
	})
	if err != nil {
		t.Fatalf("dockerOptions: %v", err)
	}
	if options.NetworkMode != "none" || options.MemoryLimitBytes != 256<<20 ||
		options.MemoryLowBytes != 64<<20 || options.MemorySwapMaxBytes != 256<<20 ||
		options.PidsLimit != 128 || math.Abs(options.CPUs-1.5) > 0.0001 || !options.ReadOnlyRootFS {
		t.Fatalf("translated Docker options = %+v", options)
	}
	if len(options.PublishedPorts) != 2 || len(options.DropCapabilities) != 1 {
		t.Fatalf("ports/capability drops = %+v / %+v", options.PublishedPorts, options.DropCapabilities)
	}
}

func TestDockerOptionsRejectsUnsupportedOrImpossibleLimits(t *testing.T) {
	tests := []struct {
		name string
		host dockerHostConfig
	}{
		{name: "swap without memory", host: dockerHostConfig{MemorySwap: 1024}},
		{name: "swap smaller than memory", host: dockerHostConfig{Memory: 2048, MemorySwap: 1024}},
		{name: "negative limit", host: dockerHostConfig{PidsLimit: -1}},
		{name: "privileged", host: dockerHostConfig{Privileged: true}},
		{name: "capability add", host: dockerHostConfig{CapAdd: []string{"SYS_ADMIN"}}},
		{name: "auto remove with restart", host: dockerHostConfig{AutoRemove: true, RestartPolicy: dockerRestartPolicyConfig{Name: "always"}}},
		{name: "unknown restart mode", host: dockerHostConfig{RestartPolicy: dockerRestartPolicyConfig{Name: "unless-stopped-and-also-maybe"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := dockerOptions(test.host); err == nil {
				t.Fatal("unsupported Docker HostConfig unexpectedly accepted")
			}
		})
	}
}

func TestParseDockerBindRequiresExistingNonSymlinkSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "data")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	mount, err := parseDockerBind(source + ":/var/lib/app:ro")
	if err != nil {
		t.Fatalf("parseDockerBind: %v", err)
	}
	if mount.Source != source || mount.Target != "/var/lib/app" || !mount.ReadOnly {
		t.Fatalf("parsed bind = %+v", mount)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := parseDockerBind(link + ":/data"); err == nil {
		t.Fatal("symlink bind source unexpectedly accepted")
	}
	if _, err := parseDockerBind(filepath.Join(root, "missing") + ":/data"); err == nil {
		t.Fatal("missing bind source unexpectedly accepted")
	}
	if _, err := parseDockerBind(source + ":/data:ro,z"); err == nil {
		t.Fatal("unsupported bind mount mode unexpectedly accepted")
	}
}

func TestDockerPortBindingsRejectEphemeralAndInvalidProtocols(t *testing.T) {
	for name, input := range map[string]map[string][]dockerPortBinding{
		"ephemeral host port": {"80/tcp": {{HostIP: "127.0.0.1"}}},
		"invalid protocol":    {"80/sctp": {{HostIP: "127.0.0.1", HostPort: "8080"}}},
		"invalid host port":   {"80/tcp": {{HostIP: "127.0.0.1", HostPort: "70000"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDockerPortBindings(input); err == nil {
				t.Fatal("invalid Docker port binding unexpectedly accepted")
			}
		})
	}
}

func TestWriteMethodErrorForSetsAllowAndJSONError(t *testing.T) {
	response := httptest.NewRecorder()
	writeMethodErrorFor(response, http.MethodPost+", "+http.MethodDelete)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if got := response.Header().Get("Allow"); got != http.MethodPost+", "+http.MethodDelete {
		t.Fatalf("Allow=%q", got)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body["message"] != "method not allowed" {
		t.Fatalf("message=%q", body["message"])
	}
}

func TestEngineAPIContainerStopRollsBackFailedSignal(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("failed-stop-signal-test", []string{"/bin/sleep", "30"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A PID outside the host PID range makes signal delivery fail without
	// involving another process or relying on a race with process exit.
	if err := state.Start(record.ID, 99999999); err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/stop?t=0")
	if response.Code != http.StatusConflict {
		t.Fatalf("stop with stale PID status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "running" || updated.StopRequested {
		t.Fatalf("failed signal left stale stop state: status=%q stopRequested=%t", updated.Status, updated.StopRequested)
	}
}

func TestEngineAPIContainerStopCancelsRestartWithoutSignalingPID(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("restart-stop-test", []string{"/bin/sleep", "30"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRestart(record.ID); err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodPost, "/containers/"+record.ID+"/stop")
	if response.Code != http.StatusNoContent {
		t.Fatalf("stop restarting container status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "exited" || !updated.StopRequested || updated.PID != 0 {
		t.Fatalf("restart was not cancelled safely: %+v", updated)
	}
}

func TestDockerContainerStartRequiresARealCreatedEngineRecord(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("not-an-engine-container", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/start", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("start without saved image/config status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/stop", nil)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotModified {
		t.Fatalf("stop of created container status=%d", response.Code)
	}
}

func TestDockerContainerStartRestartsStoppedEngineContainer(t *testing.T) {
	server := setupEngineAPITest(t)
	const reference = "registry.example/team/boxr-restart:v1"
	importEngineAPITestImage(t, reference)

	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/create?name=restartable", strings.NewReader(`{"Image":"`+reference+`"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	record, err := state.Get(created.ID)
	if err != nil {
		t.Fatalf("load created Engine API record: %v", err)
	}

	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatalf("model first start: %v", err)
	}
	if err := state.RequestStop(record.ID); err != nil {
		t.Fatalf("model manual stop: %v", err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatalf("model completed workload: %v", err)
	}

	originalLauncher := launchEngineAPICreatedContainer
	var launchedExecutable string
	var launchedArgs []string
	launchEngineAPICreatedContainer = func(executable string, args []string) (string, string, error) {
		launchedExecutable = executable
		launchedArgs = append([]string(nil), args...)
		return "", "", nil
	}
	t.Cleanup(func() { launchEngineAPICreatedContainer = originalLauncher })

	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/start")
	if response.Code != http.StatusNoContent {
		t.Fatalf("restart stopped container status=%d body=%s", response.Code, response.Body.String())
	}
	if launchedExecutable == "" {
		t.Fatal("Engine API start did not invoke the Boxr executable")
	}
	configPath, err := state.EngineConfigPath(record.ID)
	if err != nil {
		t.Fatalf("resolve Engine API start config: %v", err)
	}
	if len(launchedArgs) != 8 ||
		launchedArgs[0] != "run" ||
		launchedArgs[1] != "--config" ||
		launchedArgs[2] != configPath ||
		launchedArgs[3] != "--image" ||
		launchedArgs[4] != reference ||
		launchedArgs[5] != "--detach" ||
		launchedArgs[6] != "--internal-existing-id" ||
		launchedArgs[7] != record.ID {
		t.Fatalf("Engine API start arguments = %#v", launchedArgs)
	}
}

func TestDockerContainerRestartUsesBoundedNativeRestartCommand(t *testing.T) {
	server := setupEngineAPITest(t)
	const reference = "registry.example/team/boxr-api-restart:v1"
	importEngineAPITestImage(t, reference)

	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/create?name=api-restart", strings.NewReader(`{"Image":"`+reference+`"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	record, err := state.Get(created.ID)
	if err != nil {
		t.Fatalf("load created record: %v", err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatalf("model first start: %v", err)
	}
	if err := state.RequestStop(record.ID); err != nil {
		t.Fatalf("model stop request: %v", err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatalf("model stopped workload: %v", err)
	}

	originalLauncher := launchEngineAPICreatedContainer
	var launchedExecutable string
	var launchedArgs []string
	launchEngineAPICreatedContainer = func(executable string, args []string) (string, string, error) {
		launchedExecutable = executable
		launchedArgs = append([]string(nil), args...)
		return "", "", nil
	}
	t.Cleanup(func() { launchEngineAPICreatedContainer = originalLauncher })

	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/restart?t=5")
	if response.Code != http.StatusNoContent {
		t.Fatalf("restart status=%d body=%s", response.Code, response.Body.String())
	}
	if launchedExecutable == "" || len(launchedArgs) != 4 ||
		launchedArgs[0] != "restart" ||
		launchedArgs[1] != "--time" ||
		launchedArgs[2] != "5s" ||
		launchedArgs[3] != record.ID {
		t.Fatalf("restart command executable=%q args=%#v", launchedExecutable, launchedArgs)
	}

	for _, target := range []string{
		"/v1.45/containers/" + record.ID + "/restart?t=301",
		"/v1.45/containers/" + record.ID + "/restart?t=-2",
		"/v1.45/containers/" + record.ID + "/restart?t=5&t=10",
	} {
		response = engineAPIRequest(server, http.MethodPost, target)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", target, response.Code)
		}
	}
}

func TestEngineAPIContainerDeleteForceUsesNativeSafeRemoval(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("force-delete-restart", []string{"/bin/true"}, container.Options{
		RestartPolicy: "always",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRestart(record.ID); err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodDelete, "/v1.45/containers/"+record.ID)
	if response.Code != http.StatusConflict {
		t.Fatalf("delete without force status=%d body=%s", response.Code, response.Body.String())
	}

	originalLauncher := launchEngineAPICreatedContainer
	var launchedExecutable string
	var launchedArgs []string
	launchEngineAPICreatedContainer = func(executable string, args []string) (string, string, error) {
		launchedExecutable = executable
		launchedArgs = append([]string(nil), args...)
		if err := state.RequestStop(record.ID); err != nil {
			return "", "", err
		}
		if err := state.Remove(record.ID); err != nil {
			return "", "", err
		}
		return "", "", nil
	}
	t.Cleanup(func() { launchEngineAPICreatedContainer = originalLauncher })

	response = engineAPIRequest(server, http.MethodDelete, "/v1.45/containers/"+record.ID+"?force=true")
	if response.Code != http.StatusNoContent {
		t.Fatalf("forced delete status=%d body=%s", response.Code, response.Body.String())
	}
	if launchedExecutable == "" || len(launchedArgs) != 3 ||
		launchedArgs[0] != "rm" || launchedArgs[1] != "--force" || launchedArgs[2] != record.ID {
		t.Fatalf("forced delete command executable=%q args=%#v", launchedExecutable, launchedArgs)
	}
	if _, err := state.Get(record.ID); err == nil {
		t.Fatal("container record remains after forced Engine API delete")
	}
}

func TestDockerContainerRenameUsesUniquePersistentNames(t *testing.T) {
	server := setupEngineAPITest(t)
	first, err := state.Create("api-rename-before", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.Create("api-rename-taken", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+first.ID+"/rename?name=api-rename-after")
	if response.Code != http.StatusNoContent {
		t.Fatalf("rename status=%d body=%s", response.Code, response.Body.String())
	}
	renamed, err := state.Get("api-rename-after")
	if err != nil || renamed.ID != first.ID {
		t.Fatalf("renamed container lookup = %+v, err=%v", renamed, err)
	}

	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+first.ID+"/rename?name=api-rename-taken")
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate rename status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+first.ID+"/rename?name=../escape")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid rename status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+first.ID+"/rename")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing name status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+first.ID+"/rename?name=a&name=b")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("repeated name parameter status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/missing/rename?name=unused")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing container rename status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := state.Get(second.ID); err != nil {
		t.Fatalf("unrelated container changed after rename: %v", err)
	}
}

func TestDockerContainerWaitReturnsExitCodeAndSupportsNotRunningOnly(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("wait-exit-code", []string{"/bin/false"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, &container.ExitError{Code: 7}); err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/wait")
	if response.Code != http.StatusOK {
		t.Fatalf("wait status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		StatusCode int
		Error      any
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode wait response: %v", err)
	}
	if result.StatusCode != 7 || result.Error != nil {
		t.Fatalf("wait response = %+v", result)
	}

	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/wait?condition=next-exit")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported wait condition status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/wait?condition=not-running&condition=next-exit")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("repeated wait condition status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodGet, "/v1.45/containers/"+record.ID+"/wait")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET wait status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerContainerWaitBlocksUntilRunningContainerExits(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("wait-running", []string{"/bin/sleep", "0.1"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(20*time.Millisecond, func() {
		if err := state.Finish(record.ID, nil); err != nil {
			t.Errorf("finish waiting container: %v", err)
		}
	})
	response := engineAPIRequest(server, http.MethodPost, "/v1.45/containers/"+record.ID+"/wait")
	if response.Code != http.StatusOK {
		t.Fatalf("wait for running container status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		StatusCode int
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode wait response: %v", err)
	}
	if result.StatusCode != 0 {
		t.Fatalf("wait exit code = %d, want 0", result.StatusCode)
	}
}


func TestDockerVolumeCreateListInspectAndDelete(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))

	create := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1.45/volumes/create", strings.NewReader(body))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	response := create(`{"Name":"api-data","Driver":"local","Labels":{"team":"platform","tier":"test"}}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["Name"] != "api-data" || created["Driver"] != "local" || created["Scope"] != "local" {
		t.Fatalf("created volume response = %#v", created)
	}
	mountpoint, ok := created["Mountpoint"].(string)
	if !ok || mountpoint == "" {
		t.Fatalf("created volume has no mountpoint: %#v", created)
	}
	if info, err := os.Stat(mountpoint); err != nil || !info.IsDir() {
		t.Fatalf("volume mountpoint is not a directory: info=%v err=%v", info, err)
	}

	response = engineAPIRequest(server, http.MethodGet, "/v1.45/volumes")
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var listed dockerVolumeListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Volumes) != 1 || listed.Volumes[0].Name != "api-data" || len(listed.Warnings) != 0 {
		t.Fatalf("volume list = %#v", listed)
	}

	filters, err := json.Marshal(map[string][]string{"label": {"team=platform"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(filters)
	response = engineAPIRequest(server, http.MethodGet, "/v1.45/volumes?filters="+url.QueryEscape(encoded))
	if response.Code != http.StatusOK {
		t.Fatalf("filtered list status=%d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Volumes) != 1 || listed.Volumes[0].Name != "api-data" {
		t.Fatalf("label-filtered volumes = %#v", listed.Volumes)
	}

	response = engineAPIRequest(server, http.MethodGet, "/v1.45/volumes/api-data")
	if response.Code != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", response.Code, response.Body.String())
	}
	var inspected dockerVolumeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Name != "api-data" || inspected.Mountpoint != mountpoint {
		t.Fatalf("volume inspect = %+v", inspected)
	}

	if response = create(`{"Name":"api-data"}`); response.Code != http.StatusConflict {
		t.Fatalf("duplicate create status=%d body=%s", response.Code, response.Body.String())
	}
	if response = create(`{"Name":"bad-driver","Driver":"nfs"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported driver status=%d body=%s", response.Code, response.Body.String())
	}
	if response = create(`{"Name":"unknown-field","Unknown":true}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", response.Code, response.Body.String())
	}

	response = engineAPIRequest(server, http.MethodDelete, "/v1.45/volumes/api-data")
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodGet, "/v1.45/volumes/api-data")
	if response.Code != http.StatusNotFound {
		t.Fatalf("inspect deleted volume status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerVolumeDeleteRejectsActiveContainerUse(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	item, err := volume.Create("busy-data", map[string]string{"team": "platform"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := state.Create("volume-user", []string{"/bin/true"}, container.Options{
		BindMounts: []container.BindMount{{Source: item.Path, Target: "/data"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = state.Finish(record.ID, nil)
		_ = state.Remove(record.ID)
	}()

	response := engineAPIRequest(server, http.MethodDelete, "/v1.45/volumes/busy-data")
	if response.Code != http.StatusConflict {
		t.Fatalf("delete in-use volume status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("in-use volume data was removed: %v", err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := state.Remove(record.ID); err != nil {
		t.Fatal(err)
	}
	response = engineAPIRequest(server, http.MethodDelete, "/v1.45/volumes/busy-data")
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete released volume status=%d body=%s", response.Code, response.Body.String())
	}
}


func TestParseDockerBindAcceptsOnlyPrecreatedNamedVolumes(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	item, err := volume.Create("api-bind-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := parseDockerBind("api-bind-data:/workspace:ro")
	if err != nil {
		t.Fatalf("parse named volume bind: %v", err)
	}
	if mount.Source != item.Path || mount.Target != "/workspace" || !mount.ReadOnly {
		t.Fatalf("named volume bind = %+v; volume = %+v", mount, item)
	}
	if _, err := parseDockerBind("missing-volume:/workspace"); err == nil {
		t.Fatal("missing named volume was accepted")
	}
}

func TestDockerContainerInspectReportsNamedVolumeMount(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	item, err := volume.Create("inspect-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := state.Create("volume-inspect", []string{"/bin/true"}, container.Options{
		BindMounts: []container.BindMount{{Source: item.Path, Target: "/data", ReadOnly: true}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer state.RemoveCreated(record.ID)

	response := engineAPIRequest(server, http.MethodGet, "/v1.45/containers/"+record.ID+"/json")
	if response.Code != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", response.Code, response.Body.String())
	}
	var inspected map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	mounts, ok := inspected["Mounts"].([]any)
	if !ok || len(mounts) != 1 {
		t.Fatalf("inspect mounts = %#v", inspected["Mounts"])
	}
	mount := mounts[0].(map[string]any)
	if mount["Type"] != "volume" || mount["Name"] != "inspect-data" || mount["Source"] != item.Path || mount["RW"] != false {
		t.Fatalf("Docker mount response = %#v", mount)
	}
	hostConfig := inspected["HostConfig"].(map[string]any)
	binds := hostConfig["Binds"].([]any)
	if len(binds) != 1 || binds[0] != "inspect-data:/data:ro" {
		t.Fatalf("Docker HostConfig.Binds = %#v", binds)
	}
}
