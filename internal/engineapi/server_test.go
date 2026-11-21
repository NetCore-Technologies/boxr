package engineapi

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	boxrnetwork "github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

func setupEngineAPITest(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BOXR_STATE_DIR", root+"/state")
	t.Setenv("BOXR_IMAGE_DIR", root+"/images")
	t.Setenv("BOXR_NETWORK_DIR", root+"/networks")
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
		Hostname: "api", User: "1001:100", Workdir: "/srv/app", MemoryLimitBytes: 64 << 20,
		PublishedPorts: []container.PortMapping{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		ExposedPorts:   []string{"80/tcp", "443/tcp"},
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
	if inspect["Id"] != record.ID || inspect["Name"] != "/api-test" || config["Hostname"] != "api" || config["User"] != "1001:100" {
		t.Fatalf("container inspect = %#v", inspect)
	}
	if hostConfig["NetworkMode"] != "boxr-test" || hostConfig["Memory"] != float64(64<<20) {
		t.Fatalf("HostConfig = %#v", hostConfig)
	}
	exposedPorts, ok := config["ExposedPorts"].(map[string]any)
	if !ok || len(exposedPorts) != 2 || exposedPorts["80/tcp"] == nil || exposedPorts["443/tcp"] == nil {
		t.Fatalf("container inspect ExposedPorts = %#v", config["ExposedPorts"])
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

func TestContainerAttachStreamsDockerMultiplexedLogSnapshot(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("attach-test", []string{"/bin/echo", "hello"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("attach output\n")
	if _, err := logFile.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodPost,
		"/v1.45/containers/"+record.ID+"/attach?logs=1&stream=0&stdout=1&stderr=1")
	if response.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%q", response.Code, response.Body.String())
	}
	body := response.Body.Bytes()
	if got, want := len(body), len(payload)+8; got != want {
		t.Fatalf("attach stream length=%d want %d", got, want)
	}
	if body[0] != 1 || body[1] != 0 || body[2] != 0 || body[3] != 0 {
		t.Fatalf("unexpected Docker attach frame header: %v", body[:8])
	}
	if got := binary.BigEndian.Uint32(body[4:8]); got != uint32(len(payload)) {
		t.Fatalf("attach frame length=%d want %d", got, len(payload))
	}
	if string(body[8:]) != string(payload) {
		t.Fatalf("attach payload=%q want %q", body[8:], payload)
	}
}

func TestContainerAttachSupportsDockerTCPUpgrade(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("attach-upgrade-test", []string{"/bin/echo", "hello"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("upgraded attach output\n")
	if _, err := logFile.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	target, err := url.Parse(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	requestPath := "/v1.45/containers/" + record.ID + "/attach?logs=1&stream=0&stdout=1"
	if _, err := fmt.Fprintf(connection,
		"POST %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n",
		requestPath, target.Host); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, "101 UPGRADED") {
		t.Fatalf("Docker attach upgrade response = %q", statusLine)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	var frameHeader [8]byte
	if _, err := io.ReadFull(reader, frameHeader[:]); err != nil {
		t.Fatalf("read Docker attach frame header: %v", err)
	}
	if frameHeader[0] != 1 {
		t.Fatalf("Docker attach stream=%d, want stdout stream 1", frameHeader[0])
	}
	if got := binary.BigEndian.Uint32(frameHeader[4:]); got != uint32(len(payload)) {
		t.Fatalf("Docker attach frame length=%d want %d", got, len(payload))
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read Docker attach frame payload: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("Docker attach payload=%q want %q", got, payload)
	}
}

func TestContainerAttachForwardsDockerUpgradedStdin(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("attach-stdin-test", []string{"/bin/cat"}, container.Options{OpenStdin: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	socketPath, err := state.EngineStdinSocketPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	acceptErr := make(chan error, 1)
	go func() {
		accepted, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		defer accepted.Close()
		data, err := io.ReadAll(accepted)
		if err != nil {
			acceptErr <- err
			return
		}
		received <- string(data)
	}()

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	target, err := url.Parse(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := "/v1.45/containers/" + record.ID + "/attach?stdin=1&stream=1"
	if _, err := fmt.Fprintf(connection,
		"POST %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n",
		requestPath, target.Host); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, "101 UPGRADED") {
		_ = connection.Close()
		t.Fatalf("Docker stdin attach upgrade response = %q", statusLine)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = connection.Close()
			t.Fatal(err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	payload := "attached stdin payload\n"
	if _, err := io.WriteString(connection, payload); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != payload {
			t.Fatalf("container stdin received %q, want %q", got, payload)
		}
	case err := <-acceptErr:
		t.Fatalf("container stdin connection: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upgraded Docker stdin payload")
	}
}

func TestContainerAttachRejectsUnsupportedStdinAndMethods(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("attach-stdin-test", []string{"/bin/sleep", "1"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stdin := engineAPIRequest(server, http.MethodPost,
		"/containers/"+record.ID+"/attach?stdin=1&stream=1")
	if stdin.Code != http.StatusBadRequest {
		t.Fatalf("attach stdin status=%d body=%q", stdin.Code, stdin.Body.String())
	}
	method := engineAPIRequest(server, http.MethodGet, "/containers/"+record.ID+"/attach")
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET attach status=%d body=%q", method.Code, method.Body.String())
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
		if event["Type"] == "container" && actor["ID"] == record.ID && event["Action"] == "start" {
			if event["status"] != "start" || event["id"] != record.ID || event["scope"] != "local" {
				t.Fatalf("Docker event compatibility fields = %#v", event)
			}
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

func newEngineAPITestRegistry(t *testing.T) (*httptest.Server, string, string) {
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
		case "/v2/team/app/manifests/v1", "/v2/team/app/manifests/" + manifestDigest:
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
	return server, registryHost + "/team/app", manifestDigest
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
	registryServer, remoteImage, _ := newEngineAPITestRegistry(t)
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

func TestEngineAPIImageCreatePullsDigestPinnedOCIImageAndCreatesContainer(t *testing.T) {
	for _, mode := range []string{"fromImage", "tag"} {
		t.Run(mode, func(t *testing.T) {
			server := setupEngineAPITest(t)
			registryServer, remoteImage, manifestDigest := newEngineAPITestRegistry(t)
			digestReference := remoteImage + "@" + manifestDigest
			localReference := remoteImage + ":sha256-" + strings.TrimPrefix(manifestDigest, "sha256:")

			var target string
			switch mode {
			case "fromImage":
				target = "/v1.45/images/create?fromImage=" + url.QueryEscape(digestReference) + "&platform=linux%2Famd64"
			case "tag":
				target = "/v1.45/images/create?fromImage=" + url.QueryEscape(remoteImage) + "&tag=" + url.QueryEscape(manifestDigest) + "&platform=linux%2Famd64"
			}

			authConfig, err := json.Marshal(map[string]string{
				"username": "boxr-test", "password": "registry-secret",
				"serveraddress": registryServer.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, target, nil)
			request.Header.Set("X-Registry-Auth", base64.RawURLEncoding.EncodeToString(authConfig))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("digest image pull status=%d body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "Pull complete") || !strings.Contains(response.Body.String(), "Digest: "+manifestDigest) {
				t.Fatalf("digest pull progress stream = %s", response.Body.String())
			}
			image, err := imageStore.Inspect(localReference)
			if err != nil {
				t.Fatalf("digest-pinned image was not imported as %q: %v", localReference, err)
			}
			if image.OS != "linux" || image.Arch != "amd64" {
				t.Fatalf("digest-pinned image platform = %s/%s", image.OS, image.Arch)
			}

			created, err := createContainerRecord("digest-image-"+mode, dockerCreateRequest{Image: digestReference})
			if err != nil {
				t.Fatalf("create container from digest-pinned image reference: %v", err)
			}
			if created.Labels["io.boxr.image"] != localReference {
				t.Fatalf("container image label = %q, want stable digest alias %q", created.Labels["io.boxr.image"], localReference)
			}
			if err := state.RemoveCreated(created.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEngineAPIImageCreateRejectsInvalidQueriesAndAuth(t *testing.T) {
	server := setupEngineAPITest(t)
	for _, target := range []string{
		"/images/create",
		"/images/create?fromImage=not-a-valid-reference%20value",
		"/images/create?fromImage=example.com/team/app&tag=",
		"/images/create?fromImage=example.com/team/app%40sha256%3Abad",
		"/images/create?fromImage=example.com/team/app%40sha256%3Aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&tag=v1",
		"/images/create?fromImage=example.com/team/app&tag=sha256%3Abad",
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

func TestEngineAPIContainerDeleteAcceptsForceForCreatedContainer(t *testing.T) {
	server := setupEngineAPITest(t)
	created, err := state.Create("delete-options-test", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodDelete, "/containers/"+created.ID+"?force=true")
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

func TestDockerOptionsSupportsStructuredBindVolumeAndTmpfsMounts(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	volumeItem, err := volume.Create("structured-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	hostSource := filepath.Join(t.TempDir(), "source:with-colon")
	if err := os.Mkdir(hostSource, 0700); err != nil {
		t.Fatal(err)
	}
	input := dockerHostConfig{Mounts: []json.RawMessage{
		json.RawMessage(fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/workspace","ReadOnly":true,"BindOptions":{"Propagation":"rprivate"}}`, hostSource)),
		json.RawMessage(`{"Type":"volume","Source":"structured-data","Target":"/data","VolumeOptions":{"NoCopy":true}}`),
		json.RawMessage(`{"Type":"tmpfs","Target":"/run","TmpfsOptions":{"SizeBytes":4096,"Mode":1023}}`),
	}}
	options, err := dockerOptions(input)
	if err != nil {
		t.Fatalf("dockerOptions structured mounts: %v", err)
	}
	if len(options.BindMounts) != 2 || len(options.TmpfsMounts) != 1 {
		t.Fatalf("translated mounts = bind=%+v tmpfs=%+v", options.BindMounts, options.TmpfsMounts)
	}
	if options.BindMounts[0].Source != hostSource || options.BindMounts[0].Target != "/workspace" || !options.BindMounts[0].ReadOnly {
		t.Fatalf("bind mount translation = %+v", options.BindMounts[0])
	}
	if options.BindMounts[1].Source != volumeItem.Path || options.BindMounts[1].Target != "/data" {
		t.Fatalf("volume mount translation = %+v", options.BindMounts[1])
	}
	if options.TmpfsMounts[0].Target != "/run" || options.TmpfsMounts[0].SizeBytes != 4096 || options.TmpfsMounts[0].Mode != 1023 {
		t.Fatalf("tmpfs mount translation = %+v", options.TmpfsMounts[0])
	}
}

func TestDockerStructuredMountsRejectUnsupportedAndUnsafeOptions(t *testing.T) {
	hostSource := t.TempDir()
	for _, test := range []struct {
		name  string
		mount string
	}{
		{name: "unknown field", mount: fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/data","Unexpected":true}`, hostSource)},
		{name: "unsupported propagation", mount: fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/data","BindOptions":{"Propagation":"rshared"}}`, hostSource)},
		{name: "implicit host path creation", mount: fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/data","BindOptions":{"CreateMountpoint":true}}`, filepath.Join(t.TempDir(), "missing"))},
		{name: "volume subpath", mount: `{"Type":"volume","Source":"preexisting","Target":"/data","VolumeOptions":{"NoCopy":true,"Subpath":"child"}}`},
		{name: "tmpfs source", mount: `{"Type":"tmpfs","Source":"/tmp","Target":"/run"}`},
		{name: "tmpfs size omitted", mount: `{"Type":"tmpfs","Target":"/run"}`},
		{name: "tmpfs size too large", mount: `{"Type":"tmpfs","Target":"/run","TmpfsOptions":{"SizeBytes":1073741825}}`},
		{name: "tmpfs options", mount: `{"Type":"tmpfs","Target":"/run","TmpfsOptions":{"SizeBytes":4096,"Options":[["noexec"]]}}`},
		{name: "unsupported type", mount: fmt.Sprintf(`{"Type":"image","Source":%q,"Target":"/data"}`, hostSource)},
		{name: "target traversal", mount: fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/data/../escape"}`, hostSource)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := dockerOptions(dockerHostConfig{Mounts: []json.RawMessage{json.RawMessage(test.mount)}}); err == nil {
				t.Fatalf("unsupported structured mount unexpectedly accepted: %s", test.mount)
			}
		})
	}
	if _, err := dockerOptions(dockerHostConfig{Mounts: []json.RawMessage{json.RawMessage("{")}}); err == nil {
		t.Fatal("malformed structured mount JSON unexpectedly accepted")
	}
}

func TestDockerStructuredMountsRejectDuplicateTargetsAcrossLegacyBinds(t *testing.T) {
	hostSource := t.TempDir()
	_, err := dockerOptions(dockerHostConfig{
		Binds: []string{hostSource + ":/shared"},
		Mounts: []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/shared"}`, hostSource)),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "multiple Docker mounts target") {
		t.Fatalf("duplicate mount target error = %v", err)
	}
	_, err = dockerOptions(dockerHostConfig{Mounts: []json.RawMessage{
		json.RawMessage(fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/workspace"}`, hostSource)),
		json.RawMessage(`{"Type":"tmpfs","Target":"/workspace/cache","TmpfsOptions":{"SizeBytes":4096}}`),
	}})
	if err == nil || !strings.Contains(err.Error(), "nested Docker mount targets") {
		t.Fatalf("nested mount target error = %v", err)
	}
}

func TestDockerCreateAndInspectExposeStructuredTmpfsAndVolumeMounts(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	volumeItem, err := volume.Create("inspect-structured", nil)
	if err != nil {
		t.Fatal(err)
	}
	image := importEngineAPITestImage(t, "registry.example/team/structured-mounts:v1")
	hostSource := t.TempDir()
	mounts := []json.RawMessage{
		json.RawMessage(fmt.Sprintf(`{"Type":"bind","Source":%q,"Target":"/workspace","ReadOnly":true}`, hostSource)),
		json.RawMessage(`{"Type":"volume","Source":"inspect-structured","Target":"/data","VolumeOptions":{"NoCopy":true}}`),
		json.RawMessage(`{"Type":"tmpfs","Target":"/run","TmpfsOptions":{"SizeBytes":8192,"Mode":1023}}`),
	}
	record, err := createContainerRecord("structured-mounts", dockerCreateRequest{
		Image: image.Reference, HostConfig: dockerHostConfig{Mounts: mounts},
	})
	if err != nil {
		t.Fatalf("create container from structured Docker mounts: %v", err)
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
	mounted, ok := inspected["Mounts"].([]any)
	if !ok || len(mounted) != 3 {
		t.Fatalf("structured Docker inspect mounts = %#v", inspected["Mounts"])
	}
	types := make(map[string]map[string]any, len(mounted))
	for _, value := range mounted {
		item, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("mount entry has type %T", value)
		}
		types[item["Type"].(string)] = item
	}
	if types["bind"]["Destination"] != "/workspace" || types["bind"]["RW"] != false || types["bind"]["Propagation"] != "rprivate" {
		t.Fatalf("bind inspect data = %#v", types["bind"])
	}
	if types["volume"]["Name"] != "inspect-structured" || types["volume"]["Destination"] != "/data" {
		t.Fatalf("volume inspect data = %#v", types["volume"])
	}
	if types["tmpfs"]["Destination"] != "/run" || types["tmpfs"]["RW"] != true {
		t.Fatalf("tmpfs inspect data = %#v", types["tmpfs"])
	}
	hostConfig, ok := inspected["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("inspect HostConfig has type %T", inspected["HostConfig"])
	}
	tmpfs, ok := hostConfig["Tmpfs"].(map[string]any)
	if !ok || tmpfs["/run"] != "size=8192,mode=1777" {
		t.Fatalf("inspect HostConfig.Tmpfs = %#v", hostConfig["Tmpfs"])
	}
	if _, err := volume.Get(volumeItem.Name); err != nil {
		t.Fatalf("container inspect unexpectedly removed volume: %v", err)
	}
}

func TestDockerOptionsRejectsUnsupportedOrImpossibleLimits(t *testing.T) {
	tests := []struct {
		name string
		host dockerHostConfig
	}{
		{name: "swap without memory", host: dockerHostConfig{MemorySwap: 1024}},
		{name: "swap smaller than memory", host: dockerHostConfig{Memory: 2048, MemorySwap: 1024}},
		{name: "negative limit", host: dockerHostConfig{PidsLimit: -2}},
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

func TestDockerCreatePersistsVerifiedImageRootFSForContainerLifetime(t *testing.T) {
	server := setupEngineAPITest(t)
	const reference = "registry.example/team/persistent-rootfs:v1"
	importEngineAPITestImage(t, reference)

	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/create?name=persistent-rootfs", strings.NewReader(`{"Image":"`+reference+`","User":"1001:100"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
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
	if record.Options.User != "1001:100" {
		t.Fatalf("container User=%q, want 1001:100", record.Options.User)
	}
	expected, err := state.ManagedRootFSDir(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Options.RootFS != expected {
		t.Fatalf("container RootFS=%q, want managed persistent path %q", record.Options.RootFS, expected)
	}
	if info, err := os.Lstat(expected); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("managed image rootfs is not a real directory: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(expected, "bin", "app")); err != nil {
		t.Fatalf("unpacked image rootfs is incomplete: %v", err)
	}
	configPath, err := state.EngineConfigPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Options container.Options `json:"options"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Options.User != "1001:100" {
		t.Fatalf("saved start configuration User=%q, want 1001:100", saved.Options.User)
	}
	if saved.Options.RootFS != expected {
		t.Fatalf("saved start configuration RootFS=%q, want %q", saved.Options.RootFS, expected)
	}
	if err := state.RemoveCreated(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(expected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed image rootfs remains after deletion: %v", err)
	}
}

func TestDockerCreatePersistsManagedWritableLayerInStartConfiguration(t *testing.T) {
	server := setupEngineAPITest(t)
	const reference = "registry.example/team/persistent-writable-layer:v1"
	importEngineAPITestImage(t, reference)
	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/create?name=persistent-layer", strings.NewReader(`{"Image":"`+reference+`"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
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
	expected, err := state.ManagedOverlayDir(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Options.OverlayDir != expected {
		t.Fatalf("container OverlayDir=%q, want managed persistent path %q", record.Options.OverlayDir, expected)
	}
	configPath, err := state.EngineConfigPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Options container.Options `json:"options"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Options.OverlayDir != expected {
		t.Fatalf("saved start configuration OverlayDir=%q, want %q", saved.Options.OverlayDir, expected)
	}
	parent, err := os.Stat(filepath.Dir(expected))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0700 {
		t.Fatalf("writable-layer parent permissions=%o, want 0700", parent.Mode().Perm())
	}
	if err := state.RemoveCreated(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(expected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed writable storage remains after create rollback: %v", err)
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

func TestDockerNetworkCreateListInspectAndDelete(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := boxrnetwork.RunnerFunc(func(_ context.Context, _ string, _ ...string) error {
		return nil
	})
	server.createNetwork = func(name string, options boxrnetwork.CreateOptions) (boxrnetwork.Network, error) {
		return boxrnetwork.CreateWithRunner(context.Background(), name, options, runner)
	}
	server.removeNetwork = func(name string) error {
		return boxrnetwork.RemoveWithRunner(context.Background(), name, runner)
	}

	body := `{"Name":"api-bridge","Driver":"bridge","IPAM":{"Driver":"default","Config":[{"Subnet":"172.29.40.0/24","Gateway":"172.29.40.1"}]},"Labels":{"team":"platform","tier":"test"},"Options":{"com.docker.network.bridge.name":"brx-api-test","com.docker.network.bridge.enable_ip_masquerade":"false"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1.45/networks/create", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created dockerNetworkCreateResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Warning != "" {
		t.Fatalf("create response = %+v", created)
	}

	response = engineAPIRequest(server, http.MethodGet, "/v1.45/networks")
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var listed []dockerNetworkResponse
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID || listed[0].Name != "api-bridge" ||
		listed[0].IPAM.Config[0].Subnet != "172.29.40.0/24" || listed[0].Options["com.docker.network.bridge.name"] != "brx-api-test" {
		t.Fatalf("network list = %+v", listed)
	}

	filters, err := json.Marshal(map[string][]string{"label": {"team=platform"}, "driver": {"bridge"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(filters)
	response = engineAPIRequest(server, http.MethodGet, "/v1.45/networks?filters="+url.QueryEscape(encoded))
	if response.Code != http.StatusOK {
		t.Fatalf("filtered list status=%d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Labels["tier"] != "test" {
		t.Fatalf("filtered networks = %+v", listed)
	}

	response = engineAPIRequest(server, http.MethodGet, "/v1.45/networks/"+created.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", response.Code, response.Body.String())
	}
	var inspected dockerNetworkResponse
	if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Name != "api-bridge" || inspected.IPAM.Config[0].Gateway != "172.29.40.1" {
		t.Fatalf("network inspect = %+v", inspected)
	}

	response = engineAPIRequest(server, http.MethodDelete, "/v1.45/networks/"+created.ID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	response = engineAPIRequest(server, http.MethodGet, "/v1.45/networks/"+created.ID)
	if response.Code != http.StatusNotFound {
		t.Fatalf("inspect deleted network status=%d body=%s", response.Code, response.Body.String())
	}
}

type engineAPINetworkRunner struct {
	aliases      map[string]string
	deletedVeths []string
}

func (runner *engineAPINetworkRunner) Run(_ context.Context, executable string, args ...string) error {
	if executable == "ip" && len(args) >= 6 &&
		args[0] == "link" && args[1] == "set" && args[2] == "dev" && args[4] == "alias" {
		if runner.aliases == nil {
			runner.aliases = make(map[string]string)
		}
		runner.aliases[args[3]] = args[5]
	}
	if executable == "ip" && len(args) >= 4 && args[0] == "link" && args[1] == "del" && args[2] == "dev" {
		runner.deletedVeths = append(runner.deletedVeths, args[3])
		delete(runner.aliases, args[3])
	}
	return nil
}

func (runner *engineAPINetworkRunner) Output(_ context.Context, executable string, args ...string) ([]byte, error) {
	if executable != "ip" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected output command %q %v", executable, args)
	}
	device := args[len(args)-1]
	alias, ok := runner.aliases[device]
	if !ok {
		return nil, fmt.Errorf("Cannot find device %s", device)
	}
	return []byte(fmt.Sprintf("7: %s: <BROADCAST,UP> alias %s\n", device, alias)), nil
}

func TestDockerNetworkCreateRejectsUnsupportedModesAndOptions(t *testing.T) {
	for name, body := range map[string]string{
		"non bridge driver": `{"Name":"unsupported","Driver":"overlay","IPAM":{"Config":[{"Subnet":"172.28.40.0/24"}]}}`,
		"internal":          `{"Name":"unsupported","Driver":"bridge","Internal":true,"IPAM":{"Config":[{"Subnet":"172.28.40.0/24"}]}}`,
		"ipv6":              `{"Name":"unsupported","Driver":"bridge","EnableIPv6":true,"IPAM":{"Config":[{"Subnet":"172.28.40.0/24"}]}}`,
		"empty ipam":        `{"Name":"unsupported","Driver":"bridge","IPAM":{"Config":[]}}`,
		"auxiliary addresses": `{"Name":"unsupported","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.28.40.0/24","AuxiliaryAddresses":{"reserved":"172.28.40.10"}}]}}`,
		"unknown option":    `{"Name":"unsupported","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.28.40.0/24"}]},"Options":{"unsafe":"true"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseDockerNetworkCreate([]byte(body)); err == nil {
				t.Fatalf("unsupported network request was accepted: %s", body)
			}
		})
	}
}

func TestDockerNetworkConnectDisconnectAndInspectEndpoint(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := &engineAPINetworkRunner{aliases: make(map[string]string)}
	server.createNetwork = func(name string, options boxrnetwork.CreateOptions) (boxrnetwork.Network, error) {
		return boxrnetwork.CreateWithRunner(context.Background(), name, options, runner)
	}
	server.removeNetwork = func(name string) error {
		return boxrnetwork.RemoveWithRunner(context.Background(), name, runner)
	}
	server.joinNetwork = func(name string, pid int, id string, endpoint boxrnetwork.EndpointOptions, ports ...boxrnetwork.PortMapping) (string, error) {
		return boxrnetwork.JoinWithRunnerOptions(context.Background(), name, pid, id, runner, endpoint, ports...)
	}
	server.leaveNetwork = func(name, id string) error {
		return boxrnetwork.LeaveWithRunner(context.Background(), name, id, runner)
	}
	// The test runner substitutes the host namespace command path; the
	// production handler still validates the process through its identity hook.
	server.verifyNetworkPID = func(state.Record) error { return nil }

	createRequest := httptest.NewRequest(http.MethodPost, "/v1.45/networks/create", strings.NewReader(
		`{"Name":"connect-test","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.29.41.0/24","IPRange":"172.29.41.128/25","Gateway":"172.29.41.1"}]},"Options":{"com.docker.network.bridge.enable_ip_masquerade":"false"}}`,
	))
	createResponse := httptest.NewRecorder()
	server.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("network create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var networkCreated dockerNetworkCreateResponse
	if err := json.Unmarshal(createResponse.Body.Bytes(), &networkCreated); err != nil {
		t.Fatal(err)
	}

	record, err := state.Create("network-client", []string{"/bin/sleep", "30"}, container.Options{NetworkMode: "none"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if current, getErr := state.Get(record.ID); getErr == nil {
			if current.Status == "running" {
				_ = state.Finish(record.ID, nil)
			}
			_ = state.Remove(record.ID)
		}
	}()

	connectRequest := httptest.NewRequest(http.MethodPost, "/v1.45/networks/"+networkCreated.ID+"/connect",
		strings.NewReader(`{"Container":"`+record.ID+`","EndpointConfig":{"IPAMConfig":{"IPv4Address":"172.29.41.133"},"MacAddress":"02:aa:bb:cc:dd:ee"}}`))
	connectResponse := httptest.NewRecorder()
	server.ServeHTTP(connectResponse, connectRequest)
	if connectResponse.Code != http.StatusOK {
		t.Fatalf("network connect status=%d body=%s", connectResponse.Code, connectResponse.Body.String())
	}

	inspectResponse := engineAPIRequest(server, http.MethodGet, "/v1.45/networks/"+networkCreated.ID)
	if inspectResponse.Code != http.StatusOK {
		t.Fatalf("network inspect status=%d body=%s", inspectResponse.Code, inspectResponse.Body.String())
	}
	var inspected dockerNetworkResponse
	if err := json.Unmarshal(inspectResponse.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	endpoint, ok := inspected.Containers[record.ID]
	if !ok || endpoint["Name"] != "/network-client" || endpoint["IPv4Address"] != "172.29.41.133/24" ||
		endpoint["MacAddress"] != "02:aa:bb:cc:dd:ee" || len(endpoint["EndpointID"]) != 64 {
		t.Fatalf("network inspect endpoint = %#v", inspected.Containers)
	}
	if len(inspected.IPAM.Config) != 1 || inspected.IPAM.Config[0].IPRange != "172.29.41.128/25" {
		t.Fatalf("network inspect IPAM range = %#v", inspected.IPAM.Config)
	}

	disconnectRequest := httptest.NewRequest(http.MethodPost, "/v1.45/networks/"+networkCreated.ID+"/disconnect",
		strings.NewReader(`{"Container":"`+record.ID+`","Force":false}`))
	disconnectResponse := httptest.NewRecorder()
	server.ServeHTTP(disconnectResponse, disconnectRequest)
	if disconnectResponse.Code != http.StatusOK {
		t.Fatalf("network disconnect status=%d body=%s", disconnectResponse.Code, disconnectResponse.Body.String())
	}
	if len(runner.deletedVeths) != 1 || len(runner.aliases) != 0 {
		t.Fatalf("disconnect did not remove the verified veth: deleted=%v aliases=%v", runner.deletedVeths, runner.aliases)
	}
	details, err := boxrnetwork.InspectDetails(networkCreated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Allocations) != 0 || len(details.MACAddresses) != 0 || len(details.EndpointIDs) != 0 {
		t.Fatalf("disconnect did not persist endpoint release: allocations=%+v MACs=%+v IDs=%+v",
			details.Allocations, details.MACAddresses, details.EndpointIDs)
	}
	inspectResponse = engineAPIRequest(server, http.MethodGet, "/v1.45/networks/"+networkCreated.ID)
	if inspectResponse.Code != http.StatusOK {
		t.Fatalf("network inspect after disconnect status=%d body=%s", inspectResponse.Code, inspectResponse.Body.String())
	}
	inspected = dockerNetworkResponse{}
	if err := json.Unmarshal(inspectResponse.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if len(inspected.Containers) != 0 {
		t.Fatalf("network retained endpoint after disconnect: %#v", inspected.Containers)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := state.Remove(record.ID); err != nil {
		t.Fatal(err)
	}
	deleteResponse := engineAPIRequest(server, http.MethodDelete, "/v1.45/networks/"+networkCreated.ID)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("network delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
}

func TestVerifyDockerContainerPIDRejectsNonBoxrProcess(t *testing.T) {
	startTicks, err := state.ReadProcessStartTicks(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	record := state.Record{ID: "not-a-boxr-helper", PID: os.Getpid(), ProcessStartTicks: startTicks}
	if err := verifyDockerContainerPID(record); err == nil {
		t.Fatal("current Go test process was accepted as a Boxr namespace helper")
	}
}

func TestDockerAnonymousVolumeCreateAndPruneProtectsContainerReferences(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))

	createRequest := httptest.NewRequest(http.MethodPost, "/v1.45/volumes/create",
		strings.NewReader(`{"Driver":"local","Labels":{"purpose":"api-prune"}}`))
	createResponse := httptest.NewRecorder()
	server.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("anonymous create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created dockerVolumeResponse
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	anonymous, err := volume.Get(created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !anonymous.Anonymous || len(anonymous.Name) != 64 {
		t.Fatalf("Engine API did not create an anonymous volume: %+v", anonymous)
	}
	payload := []byte("anonymous volume data")
	if err := os.WriteFile(filepath.Join(anonymous.Path, "payload"), payload, 0600); err != nil {
		t.Fatal(err)
	}

	named, err := volume.Create("named-prune-volume", map[string]string{"purpose": "api-prune"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(named.Path, "payload"), []byte("named"), 0600); err != nil {
		t.Fatal(err)
	}
	used, err := volume.Create("referenced-prune-volume", map[string]string{"purpose": "api-prune"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := state.Create("stopped-volume-user", []string{"/bin/true"}, container.Options{
		BindMounts: []container.BindMount{{Source: used.Path, Target: "/data"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer state.RemoveCreated(record.ID)

	postPrune := func(filters map[string][]string) *httptest.ResponseRecorder {
		encodedJSON, err := json.Marshal(filters)
		if err != nil {
			t.Fatal(err)
		}
		encoded := base64.RawURLEncoding.EncodeToString(encodedJSON)
		return engineAPIRequest(server, http.MethodPost, "/v1.45/volumes/prune?filters="+url.QueryEscape(encoded))
	}

	response := postPrune(map[string][]string{"label": {"purpose=api-prune"}})
	if response.Code != http.StatusOK {
		t.Fatalf("default prune status=%d body=%s", response.Code, response.Body.String())
	}
	var first dockerVolumePruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.VolumesDeleted) != 1 || first.VolumesDeleted[0] != anonymous.Name {
		t.Fatalf("default prune must remove only the unused anonymous volume: %+v", first)
	}
	if first.SpaceReclaimed != int64(len(payload)) {
		t.Fatalf("default prune reclaimed %d bytes, want %d", first.SpaceReclaimed, len(payload))
	}
	if _, err := volume.Get(named.Name); err != nil {
		t.Fatalf("default prune removed named volume: %v", err)
	}
	if _, err := volume.Get(used.Name); err != nil {
		t.Fatalf("default prune removed a container-referenced volume: %v", err)
	}

	response = postPrune(map[string][]string{"all": {"true"}, "label": {"purpose=api-prune"}})
	if response.Code != http.StatusOK {
		t.Fatalf("all prune status=%d body=%s", response.Code, response.Body.String())
	}
	var second dockerVolumePruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.VolumesDeleted) != 1 || second.VolumesDeleted[0] != named.Name {
		t.Fatalf("all prune must remove unused named volumes but protect referenced ones: %+v", second)
	}
	if _, err := volume.Get(used.Name); err != nil {
		t.Fatalf("all prune removed a container-referenced volume: %v", err)
	}

	response = postPrune(map[string][]string{"unknown": {"value"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown prune filter status=%d body=%s", response.Code, response.Body.String())
	}
	response = postPrune(map[string][]string{"all": {"sometimes"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid all filter status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerNetworkPruneFiltersAndProtectsContainerReferences(t *testing.T) {
	server := setupEngineAPITest(t)
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := &engineAPINetworkRunner{aliases: make(map[string]string)}
	server.createNetwork = func(name string, options boxrnetwork.CreateOptions) (boxrnetwork.Network, error) {
		return boxrnetwork.CreateWithRunner(context.Background(), name, options, runner)
	}
	server.removeNetwork = func(name string) error {
		return boxrnetwork.RemoveWithRunner(context.Background(), name, runner)
	}
	createNetwork := func(name, subnet, label string) {
		t.Helper()
		_, err := server.createNetwork(name, boxrnetwork.CreateOptions{
			Subnet: subnet, Gateway: strings.TrimSuffix(subnet, "0/24") + "1",
			NAT: false, Labels: map[string]string{"team": label},
		})
		if err != nil {
			t.Fatalf("create network %s: %v", name, err)
		}
	}

	createNetwork("prune-candidate", "172.29.50.0/24", "prune")
	createNetwork("keep-network", "172.29.51.0/24", "prune")
	createNetwork("other-network", "172.29.52.0/24", "other")
	record, err := state.Create("network-config-reference", []string{"/bin/true"}, container.Options{
		NetworkMode: "bridge", NetworkName: "keep-network",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer state.RemoveCreated(record.ID)

	postPrune := func(filters map[string][]string) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(filters)
		if err != nil {
			t.Fatal(err)
		}
		encoded := base64.RawURLEncoding.EncodeToString(data)
		return engineAPIRequest(server, http.MethodPost, "/v1.45/networks/prune?filters="+url.QueryEscape(encoded))
	}

	response := postPrune(map[string][]string{"label": {"team=prune"}})
	if response.Code != http.StatusOK {
		t.Fatalf("label prune status=%d body=%s", response.Code, response.Body.String())
	}
	var first dockerNetworkPruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.NetworksDeleted) != 1 || first.NetworksDeleted[0] != "prune-candidate" {
		t.Fatalf("label prune deleted %v; expected only prune-candidate", first.NetworksDeleted)
	}
	if _, err := boxrnetwork.Inspect("keep-network"); err != nil {
		t.Fatalf("network referenced by a persisted container was pruned: %v", err)
	}
	if _, err := boxrnetwork.Inspect("other-network"); err != nil {
		t.Fatalf("network with a non-matching label was pruned: %v", err)
	}

	response = postPrune(map[string][]string{"label!": {"team=prune"}})
	if response.Code != http.StatusOK {
		t.Fatalf("negative label prune status=%d body=%s", response.Code, response.Body.String())
	}
	var second dockerNetworkPruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.NetworksDeleted) != 1 || second.NetworksDeleted[0] != "other-network" {
		t.Fatalf("negative label prune deleted %v; expected only other-network", second.NetworksDeleted)
	}

	createNetwork("until-network", "172.29.53.0/24", "old")
	futureTimestamp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	response = postPrune(map[string][]string{"until": {futureTimestamp}})
	if response.Code != http.StatusOK {
		t.Fatalf("until prune status=%d body=%s", response.Code, response.Body.String())
	}
	var third dockerNetworkPruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &third); err != nil {
		t.Fatal(err)
	}
	if len(third.NetworksDeleted) != 1 || third.NetworksDeleted[0] != "until-network" {
		t.Fatalf("until prune deleted %v; expected until-network", third.NetworksDeleted)
	}
	if _, err := boxrnetwork.Inspect("keep-network"); err != nil {
		t.Fatalf("until prune removed a network still referenced by a container: %v", err)
	}

	response = postPrune(map[string][]string{"unknown": {"value"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown filter status=%d body=%s", response.Code, response.Body.String())
	}
	response = postPrune(map[string][]string{"until": {"not-a-timestamp"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid until filter status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestParseDockerNetworkUntilAcceptsDockerTimestampForms(t *testing.T) {
	now := time.Date(2026, time.November, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		value string
		want  time.Time
	}{
		{"1795262400", time.Unix(1795262400, 0)},
		{"1795262400.125", time.Unix(1795262400, 125000000)},
		{"1h", now.Add(-time.Hour)},
		{"2026-11-21T11:00:00Z", time.Date(2026, time.November, 21, 11, 0, 0, 0, time.UTC)},
	}
	for _, test := range cases {
		got, err := parseDockerNetworkUntil(test.value, now)
		if err != nil {
			t.Errorf("parse %q: %v", test.value, err)
			continue
		}
		if !got.Equal(test.want) {
			t.Errorf("parse %q = %s, want %s", test.value, got, test.want)
		}
	}
}

func TestEngineAPIImageSaveAndLoadDockerArchives(t *testing.T) {
	server := setupEngineAPITest(t)
	const reference = "registry.example/team/boxr-archive:v1"
	const alias = "registry.example/team/boxr-archive:stable"
	importEngineAPITestImage(t, reference)
	if err := imageStore.Tag(reference, alias); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "saved.tar")
	if err := imageStore.SaveDockerMany([]string{reference, alias}, archivePath); err != nil {
		t.Fatalf("save Docker archive fixture: %v", err)
	}
	tags, err := imageStore.DockerArchiveTags(archivePath)
	if err != nil {
		t.Fatalf("preflight Docker archive tags: %v", err)
	}
	if len(tags) != 2 || tags[0] != alias || tags[1] != reference {
		t.Fatalf("Docker archive tags = %v", tags)
	}
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	record, err := state.Create("docker-load-user", []string{"/bin/app"}, container.Options{}, map[string]string{"io.boxr.image": reference})
	if err != nil {
		t.Fatal(err)
	}
	loadRequest := func(body []byte, query string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1.45/images/load"+query, bytes.NewReader(body))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	response := loadRequest(archiveData, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("loading an archive that retags an in-use image status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := imageStore.Inspect(reference); err != nil {
		t.Fatalf("in-use image tag was changed by rejected load: %v", err)
	}
	if err := state.RemoveCreated(record.ID); err != nil {
		t.Fatal(err)
	}

	response = loadRequest(archiveData, "")
	if response.Code != http.StatusOK {
		t.Fatalf("Docker image load status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Loaded image: "+reference) ||
		!strings.Contains(response.Body.String(), "Loaded image: "+alias) {
		t.Fatalf("Docker image load progress = %s", response.Body.String())
	}
	if _, err := imageStore.Inspect(reference); err != nil {
		t.Fatalf("loaded primary image not found: %v", err)
	}
	if _, err := imageStore.Inspect(alias); err != nil {
		t.Fatalf("loaded alias image not found: %v", err)
	}

	response = engineAPIRequest(server, http.MethodGet, "/v1.45/images/"+reference+"/get")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("single image export status=%d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), "boxr-images.tar") || response.Body.Len() == 0 {
		t.Fatalf("single image export headers or body missing: headers=%v bytes=%d", response.Header(), response.Body.Len())
	}
	exportedPath := filepath.Join(t.TempDir(), "exported.tar")
	if err := os.WriteFile(exportedPath, response.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	exportedTags, err := imageStore.DockerArchiveTags(exportedPath)
	if err != nil {
		t.Fatalf("verify exported Docker archive: %v", err)
	}
	if len(exportedTags) != 1 || exportedTags[0] != reference {
		t.Fatalf("single image export tags = %v", exportedTags)
	}

	query := url.Values{}
	query.Add("names", reference)
	query.Add("names", alias)
	response = engineAPIRequest(server, http.MethodGet, "/v1.45/images/get?"+query.Encode())
	if response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("multi-image export status=%d bodyBytes=%d body=%s", response.Code, response.Body.Len(), response.Body.String())
	}
	exportedPath = filepath.Join(t.TempDir(), "exported-many.tar")
	if err := os.WriteFile(exportedPath, response.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	exportedTags, err = imageStore.DockerArchiveTags(exportedPath)
	if err != nil {
		t.Fatalf("verify multi-image Docker archive: %v", err)
	}
	if len(exportedTags) != 2 || exportedTags[0] != alias || exportedTags[1] != reference {
		t.Fatalf("multi-image export tags = %v", exportedTags)
	}

	response = loadRequest(archiveData, "?quiet=1")
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("quiet Docker image load status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEngineAPIImageArchiveEndpointsRejectInvalidInput(t *testing.T) {
	server := setupEngineAPITest(t)
	for _, target := range []string{
		"/v1.45/images/get",
		"/v1.45/images/get?names=",
		"/v1.45/images/registry.example/team/missing:v1/get",
	} {
		response := engineAPIRequest(server, http.MethodGet, target)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
			t.Errorf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1.45/images/load", strings.NewReader("not a Docker archive"))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid Docker image archive status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1.45/images/load?quiet=maybe", strings.NewReader("archive"))
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid quiet parameter status=%d body=%s", response.Code, response.Body.String())
	}
}

func dockerBuildTestContext(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		header := &tar.Header{
			Name: name, Mode: 0644, Size: int64(len(data)),
			Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buffer.Bytes()...)
}

func TestEngineAPIDockerBuildCreatesAndTagsVerifiedImage(t *testing.T) {
	server := setupEngineAPITest(t)
	contextArchive := dockerBuildTestContext(t, map[string][]byte{
		"Dockerfile": []byte("FROM scratch\nARG BUILD_VERSION\nCOPY app /usr/bin/app\nENV BUILD_VERSION=$BUILD_VERSION\nLABEL org.example.builder=boxr\n"),
		"app":        []byte("built through Docker Engine API\n"),
	})
	query := url.Values{}
	query.Add("t", "registry.example/team/boxr-build:v1")
	query.Add("t", "registry.example/team/boxr-build:stable")
	query.Set("platform", "linux/amd64")
	query.Set("networkmode", "default")
	query.Set("buildargs", `{"BUILD_VERSION":"1.2.3"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1.45/build?"+query.Encode(), bytes.NewReader(contextArchive))
	request.Header.Set("Content-Type", "application/x-tar")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Docker build status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Successfully built") ||
		!strings.Contains(response.Body.String(), "Successfully tagged registry.example/team/boxr-build:v1") ||
		!strings.Contains(response.Body.String(), "Successfully tagged registry.example/team/boxr-build:stable") {
		t.Fatalf("Docker build stream = %s", response.Body.String())
	}
	primary, err := imageStore.Verify("registry.example/team/boxr-build:v1")
	if err != nil {
		t.Fatalf("inspect primary built image: %v", err)
	}
	alias, err := imageStore.Verify("registry.example/team/boxr-build:stable")
	if err != nil {
		t.Fatalf("inspect tagged built image: %v", err)
	}
	if primary.Digest != alias.Digest {
		t.Fatalf("tagged images differ: primary=%s alias=%s", primary.Digest, alias.Digest)
	}
	if primary.RuntimeConfig.Env == nil || !strings.Contains(strings.Join(primary.RuntimeConfig.Env, "\n"), "BUILD_VERSION=1.2.3") {
		t.Fatalf("build arg was not applied to Dockerfile ENV: %+v", primary.RuntimeConfig.Env)
	}
	if primary.RuntimeConfig.Labels["org.example.builder"] != "boxr" {
		t.Fatalf("Dockerfile labels = %+v", primary.RuntimeConfig.Labels)
	}
	root := filepath.Join(t.TempDir(), "rootfs")
	if _, err := imageStore.Unpack(primary.Reference, root); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "usr", "bin", "app")); err != nil || string(data) != "built through Docker Engine API\n" {
		t.Fatalf("built rootfs file = %q, err=%v", data, err)
	}
}

func TestEngineAPIDockerBuildProtectsInUseTagsAndSupportsQuiet(t *testing.T) {
	server := setupEngineAPITest(t)
	reference := "registry.example/team/boxr-build-protected:v1"
	contextArchive := dockerBuildTestContext(t, map[string][]byte{
		"Dockerfile": []byte("FROM scratch\nCOPY app /app\n"),
		"app":        []byte("first version\n"),
	})
	buildRequest := func(tag string, context []byte, quiet bool) *httptest.ResponseRecorder {
		query := url.Values{}
		query.Set("t", tag)
		if quiet {
			query.Set("q", "1")
		}
		request := httptest.NewRequest(http.MethodPost, "/v1.45/build?"+query.Encode(), bytes.NewReader(context))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	response := buildRequest(reference, contextArchive, true)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("quiet Docker build status=%d body=%s", response.Code, response.Body.String())
	}
	before, err := imageStore.Verify(reference)
	if err != nil {
		t.Fatal(err)
	}
	record, err := state.Create("build-tag-user", []string{"/app"}, container.Options{}, map[string]string{"io.boxr.image": reference})
	if err != nil {
		t.Fatal(err)
	}
	defer state.RemoveCreated(record.ID)
	changedContext := dockerBuildTestContext(t, map[string][]byte{
		"Dockerfile": []byte("FROM scratch\nCOPY app /app\n"),
		"app":        []byte("second version\n"),
	})
	response = buildRequest(reference, changedContext, false)
	if response.Code != http.StatusConflict {
		t.Fatalf("build replacing an in-use tag status=%d body=%s", response.Code, response.Body.String())
	}
	after, err := imageStore.Verify(reference)
	if err != nil {
		t.Fatal(err)
	}
	if after.Digest != before.Digest {
		t.Fatalf("in-use image tag was replaced: before=%s after=%s", before.Digest, after.Digest)
	}
}

func TestEngineAPIDockerBuildRejectsInvalidQueriesAndUnsafeContexts(t *testing.T) {
	server := setupEngineAPITest(t)
	safeContext := dockerBuildTestContext(t, map[string][]byte{"Dockerfile": []byte("FROM scratch\n")})
	for _, query := range []string{
		"/v1.45/build",
		"/v1.45/build?t=",
		"/v1.45/build?t=invalid%20reference",
		"/v1.45/build?t=demo%3Alatest&dockerfile=..%2Foutside",
		"/v1.45/build?t=demo%3Alatest&networkmode=bridge",
		"/v1.45/build?t=demo%3Alatest&rm=0",
		"/v1.45/build?t=demo%3Alatest&pull=1",
		"/v1.45/build?t=demo%3Alatest&nocache=1",
		"/v1.45/build?t=demo%3Alatest&unknown=value",
		"/v1.45/build?t=demo%3Alatest&buildargs=not-json",
	} {
		request := httptest.NewRequest(http.MethodPost, query, bytes.NewReader(safeContext))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s", query, response.Code, response.Body.String())
		}
	}

	unsafeContext := dockerBuildTestContext(t, map[string][]byte{
		"../escape":  []byte("outside"),
		"Dockerfile": []byte("FROM scratch\n"),
	})
	request := httptest.NewRequest(http.MethodPost, "/v1.45/build?t=demo%3Alatest", bytes.NewReader(unsafeContext))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("traversal build context status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerImagePruneProtectsContainerDigestsAndRemovesUnusedTags(t *testing.T) {
	server := setupEngineAPITest(t)
	createImage := func(reference, contents, labelValue string) imageStore.Image {
		t.Helper()
		root := filepath.Join(t.TempDir(), "rootfs")
		if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "bin", "app"), []byte(contents), 0755); err != nil {
			t.Fatal(err)
		}
		created, err := imageStore.ImportRootFSWithConfig(root, reference, "linux", "amd64", "", imageStore.ImageRuntimeConfig{
			Entrypoint: []string{"/bin/app"},
			Labels:     map[string]string{"org.example.prune": labelValue},
		})
		if err != nil {
			t.Fatalf("import image %s: %v", reference, err)
		}
		return created
	}
	used := createImage("registry.example/team/prune-used:latest", "container image\n", "shared")
	alias := "registry.example/team/prune-used:stable"
	if err := imageStore.Tag(used.Reference, alias); err != nil {
		t.Fatal(err)
	}
	unused := createImage("registry.example/team/prune-unused:latest", "unused image\n", "prunable")
	record, err := state.Create("prune-protected-container", []string{"/bin/app"}, container.Options{}, map[string]string{
		"io.boxr.image": used.Reference,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer state.RemoveCreated(record.ID)

	filters, err := json.Marshal(map[string][]string{
		"dangling": {"false"},
		"until":    {"9999999999"},
		"label":    {"org.example.prune=prunable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(filters)
	response := engineAPIRequest(server, http.MethodPost, "/v1.45/images/prune?filters="+url.QueryEscape(encoded))
	if response.Code != http.StatusOK {
		t.Fatalf("image prune status=%d body=%s", response.Code, response.Body.String())
	}
	var pruned dockerImagePruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &pruned); err != nil {
		t.Fatal(err)
	}
	foundUntagged := false
	foundDeletedBlob := false
	for _, item := range pruned.ImagesDeleted {
		if item.Untagged == unused.Reference {
			foundUntagged = true
		}
		if item.Deleted != "" {
			foundDeletedBlob = true
		}
		if item.Untagged == used.Reference || item.Untagged == alias {
			t.Fatalf("prune removed container-used image tag %q", item.Untagged)
		}
	}
	if !foundUntagged || !foundDeletedBlob || pruned.SpaceReclaimed <= 0 {
		t.Fatalf("image prune report = %+v", pruned)
	}
	for _, reference := range []string{used.Reference, alias} {
		image, err := imageStore.Verify(reference)
		if err != nil || image.Digest != used.Digest {
			t.Fatalf("protected image %s changed: image=%+v err=%v", reference, image, err)
		}
	}
	if _, err := imageStore.Inspect(unused.Reference); err == nil {
		t.Fatal("unused image tag remains after prune")
	}
}

func TestDockerImagePruneDefaultsToDanglingOnlyAndValidatesFilters(t *testing.T) {
	server := setupEngineAPITest(t)
	image := importEngineAPITestImage(t, "registry.example/team/prune-default:latest")
	response := engineAPIRequest(server, http.MethodPost, "/v1.45/images/prune")
	if response.Code != http.StatusOK {
		t.Fatalf("default image prune status=%d body=%s", response.Code, response.Body.String())
	}
	var pruned dockerImagePruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &pruned); err != nil {
		t.Fatal(err)
	}
	for _, item := range pruned.ImagesDeleted {
		if item.Untagged == image.Reference {
			t.Fatalf("default dangling-only prune removed tagged image %q", image.Reference)
		}
	}
	if _, err := imageStore.Verify(image.Reference); err != nil {
		t.Fatalf("default prune removed tagged image: %v", err)
	}

	for name, filters := range map[string]map[string][]string{
		"unknown key":      {"unsupported": {"yes"}},
		"invalid dangling": {"dangling": {"maybe"}},
		"invalid until":    {"until": {"not-a-time"}},
		"empty label":      {"label": {""}},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(filters)
			if err != nil {
				t.Fatal(err)
			}
			encoded := base64.RawURLEncoding.EncodeToString(data)
			response := engineAPIRequest(server, http.MethodPost, "/v1.45/images/prune?filters="+url.QueryEscape(encoded))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid image prune filter status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDockerStatusWithHealthMatchesDockerPSHealthSuffix(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
		health *state.HealthState
		want   string
	}{
		{name: "healthy", status: "Up 3 minutes", health: &state.HealthState{Status: "healthy"}, want: "Up 3 minutes (healthy)"},
		{name: "unhealthy", status: "Up 3 minutes", health: &state.HealthState{Status: "unhealthy"}, want: "Up 3 minutes (unhealthy)"},
		{name: "starting", status: "Up 3 minutes", health: &state.HealthState{Status: "starting"}, want: "Up 3 minutes (health: starting)"},
		{name: "stopped", status: "Exited (0) 2 seconds ago", health: &state.HealthState{Status: "healthy"}, want: "Exited (0) 2 seconds ago"},
		{name: "no healthcheck", status: "Up 3 minutes", want: "Up 3 minutes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := dockerStatusWithHealth(test.status, test.health); got != test.want {
				t.Fatalf("dockerStatusWithHealth() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDockerNetworkAttachmentsExposeVerifiedIPv4Leases(t *testing.T) {
	record := state.Record{
		ID: "container-123", Name: "api",
		Options: container.Options{NetworkName: "frontend"},
	}
	networks := []boxrnetwork.Network{
		{
			ID: "network-frontend-id", Name: "frontend", Driver: "bridge",
			Bridge: "br-boxr0", Subnet: "10.24.0.0/24", Gateway: "10.24.0.1",
			Allocations:  map[string]string{record.ID: "10.24.0.5", "stale-container": "10.24.0.6"},
			MACAddresses: map[string]string{record.ID: "02:11:22:33:44:55"},
			EndpointIDs:  map[string]string{record.ID: strings.Repeat("ab", 32)},
		},
		{
			ID: "network-backend-id", Name: "backend", Driver: "bridge",
			Bridge: "br-boxr1", Subnet: "10.25.0.0/24", Gateway: "10.25.0.1",
			Allocations: map[string]string{record.ID: "10.25.0.5"},
		},
	}
	attachments, err := dockerNetworkAttachmentsFromNetworks(networks, []state.Record{record})
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments[record.ID]) != 2 {
		t.Fatalf("container network attachment count=%d, want 2: %#v", len(attachments[record.ID]), attachments[record.ID])
	}
	frontend := attachments[record.ID]["frontend"].(map[string]any)
	if frontend["NetworkID"] != "network-frontend-id" ||
		frontend["Gateway"] != "10.24.0.1" ||
		frontend["IPAddress"] != "10.24.0.5" ||
		frontend["IPPrefixLen"] != 24 ||
		frontend["MacAddress"] != "02:11:22:33:44:55" ||
		frontend["EndpointID"] != strings.Repeat("ab", 32) {
		t.Fatalf("Docker frontend network metadata = %#v", frontend)
	}
	if _, exists := attachments["stale-container"]; exists {
		t.Fatal("network lease for a missing container was exposed")
	}
	settings := dockerContainerNetworkSettings(record, map[string][]map[string]string{}, attachments)
	if settings["IPAddress"] != "10.24.0.5" || settings["Gateway"] != "10.24.0.1" {
		t.Fatalf("Docker primary network settings = %#v", settings)
	}
	if len(settings["Networks"].(map[string]any)) != 2 {
		t.Fatalf("Docker inspect did not expose both attached networks: %#v", settings["Networks"])
	}
}

func TestDockerNetworkAttachmentsRejectCorruptPersistedLeases(t *testing.T) {
	record := state.Record{ID: "container-corrupt"}
	network := boxrnetwork.Network{
		ID: "network-id", Name: "frontend", Subnet: "10.24.0.0/24",
		Gateway:     "10.24.0.1",
		Allocations: map[string]string{record.ID: "192.0.2.8"},
	}
	if _, err := dockerNetworkAttachmentsFromNetworks([]boxrnetwork.Network{network}, []state.Record{record}); err == nil {
		t.Fatal("out-of-subnet container IPAM lease was accepted")
	}
}

func TestDockerNetworkAttachmentsReturnEmptyMapsForUnattachedContainers(t *testing.T) {
	record := state.Record{ID: "container-unattached"}
	attachments, err := dockerNetworkAttachmentsFromNetworks(nil, []state.Record{record})
	if err != nil {
		t.Fatal(err)
	}
	if attachments[record.ID] == nil || len(attachments[record.ID]) != 0 {
		t.Fatalf("unattached container network map = %#v, want an empty object", attachments[record.ID])
	}
	settings := dockerContainerNetworkSettings(record, nil, attachments)
	if networks, ok := settings["Networks"].(map[string]any); !ok || len(networks) != 0 {
		t.Fatalf("Docker inspect networks = %#v, want empty object", settings["Networks"])
	}
}

func TestDockerContainerFiltersMatchNetworkHealthExitAndLabelExclusion(t *testing.T) {
	exitCode := 1
	record := state.Record{
		ID: "container-filter-test", Status: "exited", ExitCode: &exitCode,
		Labels: map[string]string{"team": "platform", "tier": "api"},
		Health: &state.HealthState{Status: "unhealthy"},
	}
	networks := map[string]any{
		"frontend": map[string]any{"NetworkID": "network-frontend-identifier"},
	}
	for _, filter := range []struct {
		name   string
		values map[string][]string
		want   bool
	}{
		{name: "network name", values: map[string][]string{"network": {"frontend"}}, want: true},
		{name: "network ID prefix", values: map[string][]string{"network": {"network-front"}}, want: true},
		{name: "network mismatch", values: map[string][]string{"network": {"backend"}}, want: false},
		{name: "health", values: map[string][]string{"health": {"unhealthy"}}, want: true},
		{name: "health mismatch", values: map[string][]string{"health": {"healthy"}}, want: false},
		{name: "exit code", values: map[string][]string{"exited": {"1"}}, want: true},
		{name: "exit code mismatch", values: map[string][]string{"exited": {"0"}}, want: false},
		{name: "label exclusion", values: map[string][]string{"label!": {"team=platform"}}, want: false},
		{name: "label exclusion non-match", values: map[string][]string{"label!": {"team=database"}}, want: true},
		{name: "multiple filters are conjunctive", values: map[string][]string{"network": {"frontend"}, "health": {"unhealthy"}, "exited": {"1"}}, want: true},
	} {
		t.Run(filter.name, func(t *testing.T) {
			if got := matchesContainerFilters(record, filter.values, networks); got != filter.want {
				t.Fatalf("matchesContainerFilters()=%t, want %t", got, filter.want)
			}
		})
	}
}

func TestDockerNetworkInspectIncludesEndpointIdentity(t *testing.T) {
	record := state.Record{ID: "container-endpoint", Name: "gateway"}
	network := boxrnetwork.Network{
		ID: "network-endpoint", Name: "frontend", Driver: "bridge",
		Bridge: "brxendpoint", Subnet: "10.40.0.0/24", Gateway: "10.40.0.1",
		Allocations:  map[string]string{record.ID: "10.40.0.4"},
		MACAddresses: map[string]string{record.ID: "02:aa:bb:cc:dd:ee"},
		EndpointIDs:  map[string]string{record.ID: strings.Repeat("cd", 32)},
	}
	response := dockerNetworkWithContainers(network, []state.Record{record})
	endpoint := response.Containers[record.ID]
	if endpoint["EndpointID"] != strings.Repeat("cd", 32) ||
		endpoint["MacAddress"] != "02:aa:bb:cc:dd:ee" ||
		endpoint["IPv4Address"] != "10.40.0.4/24" ||
		endpoint["Name"] != "/gateway" {
		t.Fatalf("Docker network inspect endpoint = %#v", endpoint)
	}
}

func TestParseDockerEndpointConfigSupportsStaticIPv4AndMAC(t *testing.T) {
	raw := json.RawMessage(`{"IPAMConfig":{"IPv4Address":"172.30.48.5"},"MacAddress":"02:aa:bb:cc:dd:ee"}`)
	endpoint, err := parseDockerEndpointConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.IPv4Address != "172.30.48.5" || endpoint.MACAddress != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("parsed endpoint options = %+v", endpoint)
	}
}

func TestParseDockerEndpointConfigRejectsUnsupportedAndMalformedFields(t *testing.T) {
	for _, raw := range []string{
		`{"Aliases":["api"]}`,
		`{"Links":["db:database"]}`,
		`{"DriverOpts":{"mtu":"1400"}}`,
		`{"IPAMConfig":{"IPv6Address":"2001:db8::4"}}`,
		`{"IPAMConfig":{"LinkLocalIPs":["169.254.1.1"]}}`,
		`{"MacAddress":"ff:ff:ff:ff:ff:ff"}`,
		`{"Unexpected":true}`,
		`{"MacAddress":"02:aa:bb:cc:dd:ee"} { }`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseDockerEndpointConfig(json.RawMessage(raw)); err == nil {
				t.Fatalf("unsupported or malformed EndpointConfig %s was accepted", raw)
			}
		})
	}
}
