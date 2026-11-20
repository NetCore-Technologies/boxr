package engineapi

import (
	"encoding/base64"
	"encoding/binary"
	"bufio"
	"encoding/json"
	"strconv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
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
		BindMounts: []container.BindMount{{Source: "/srv/data", Target: "/data", ReadOnly: true}},
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
