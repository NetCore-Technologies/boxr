package engineapi

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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
	response := engineAPIRequest(server, http.MethodGet, "/images/not-a-tag/json")
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
