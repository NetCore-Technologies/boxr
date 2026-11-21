package engineapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func execAPITestContainer(t *testing.T, server *Server) state.Record {
	t.Helper()
	server.verifyExecPID = func(state.Record) error { return nil }
	return createEngineAPITestContainer(t, "exec-api-test", container.Options{})
}

func postDockerExecCreate(t *testing.T, server *Server, containerID string, config dockerExecCreateConfig) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/"+containerID+"/exec", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func TestDockerEngineExecCreateStartStreamAndInspect(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	response := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"/bin/example", "hello"},
		Env:          []string{"APP_MODE=test"},
		WorkingDir:   "/srv/app",
		User:         "root",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("exec create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Id) != 64 {
		t.Fatalf("exec ID length = %d, want 64", len(created.Id))
	}

	originalRunner := dockerExecRunner
	t.Cleanup(func() { dockerExecRunner = originalRunner })
	var sawConfig bool
	dockerExecRunner = func(_ context.Context, containerID string, config dockerExecCreateConfig, stdout, stderr io.Writer, onStart func(int)) (int, error) {
		sawConfig = containerID == record.ID && strings.Join(config.Cmd, "|") == "/bin/example|hello" &&
			config.WorkingDir == "/srv/app" && config.User == "root" && len(config.Env) == 1 && config.Env[0] == "APP_MODE=test"
		onStart(4321)
		if _, err := io.WriteString(stdout, "exec stdout\n"); err != nil {
			return 125, err
		}
		if _, err := io.WriteString(stderr, "exec stderr\n"); err != nil {
			return 125, err
		}
		return 23, nil
	}

	start := httptest.NewRequest(http.MethodPost, "/v1.45/exec/"+created.Id+"/start", strings.NewReader("{}"))
	startResponse := httptest.NewRecorder()
	server.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusOK {
		t.Fatalf("exec start status=%d body=%s", startResponse.Code, startResponse.Body.String())
	}
	if got := startResponse.Header().Get("Content-Type"); got != "application/vnd.docker.raw-stream" {
		t.Fatalf("exec stream Content-Type=%q", got)
	}
	stdout, stderr := decodeDockerExecFrames(t, startResponse.Body.Bytes())
	if stdout != "exec stdout\n" || stderr != "exec stderr\n" || !sawConfig {
		t.Fatalf("exec streams/config stdout=%q stderr=%q config=%t", stdout, stderr, sawConfig)
	}

	inspect := engineAPIRequest(server, http.MethodGet, "/v1.45/exec/"+created.Id+"/json")
	if inspect.Code != http.StatusOK {
		t.Fatalf("exec inspect status=%d body=%s", inspect.Code, inspect.Body.String())
	}
	var result struct {
		ID            string
		Running       bool
		ExitCode      *int
		ContainerID   string
		Pid           int
		ProcessConfig struct {
			User string
		}
	}
	if err := json.Unmarshal(inspect.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != created.Id || result.Running || result.ExitCode == nil || *result.ExitCode != 23 ||
		result.ContainerID != record.ID || result.Pid != 4321 || result.ProcessConfig.User != "root" {
		t.Fatalf("exec inspect result = %+v", result)
	}
	secondStart := engineAPIRequest(server, http.MethodPost, "/exec/"+created.Id+"/start")
	if secondStart.Code != http.StatusConflict {
		t.Fatalf("second exec start status=%d body=%s", secondStart.Code, secondStart.Body.String())
	}
}

func TestDockerEngineExecDetachedSessionCanBeInspectedWhileRunning(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	response := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{Cmd: []string{"/bin/long-task"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("exec create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	originalRunner := dockerExecRunner
	t.Cleanup(func() { dockerExecRunner = originalRunner })
	started := make(chan struct{})
	release := make(chan struct{})
	dockerExecRunner = func(_ context.Context, _ string, _ dockerExecCreateConfig, _, _ io.Writer, onStart func(int)) (int, error) {
		onStart(7654)
		close(started)
		<-release
		return 0, nil
	}
	startRequest := httptest.NewRequest(http.MethodPost, "/exec/"+created.Id+"/start", strings.NewReader(`{"Detach":true}`))
	start := httptest.NewRecorder()
	server.ServeHTTP(start, startRequest)
	if start.Code != http.StatusOK || start.Body.Len() != 0 {
		t.Fatalf("detached start status=%d body=%s", start.Code, start.Body.String())
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("detached exec runner did not start")
	}
	inspect := engineAPIRequest(server, http.MethodGet, "/exec/"+created.Id+"/json")
	var running struct {
		Running bool
		Pid     int
	}
	if err := json.Unmarshal(inspect.Body.Bytes(), &running); err != nil {
		t.Fatal(err)
	}
	if !running.Running || running.Pid != 7654 {
		t.Fatalf("running detached exec state = %+v", running)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		inspect = engineAPIRequest(server, http.MethodGet, "/exec/"+created.Id+"/json")
		var complete struct {
			Running  bool
			ExitCode *int
		}
		if err := json.Unmarshal(inspect.Body.Bytes(), &complete); err != nil {
			t.Fatal(err)
		}
		if !complete.Running && complete.ExitCode != nil && *complete.ExitCode == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("detached exec session did not record completion")
}

func TestDockerEngineExecRejectsUnsupportedOrUnsafeCreateOptions(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	cases := []struct {
		name   string
		config dockerExecCreateConfig
	}{
		{name: "stdin", config: dockerExecCreateConfig{AttachStdin: true, Cmd: []string{"/bin/true"}}},
		{name: "tty", config: dockerExecCreateConfig{Tty: true, Cmd: []string{"/bin/true"}}},
		{name: "privileged", config: dockerExecCreateConfig{Privileged: true, Cmd: []string{"/bin/true"}}},
		{name: "invalid user", config: dockerExecCreateConfig{User: "bad user", Cmd: []string{"/bin/true"}}},
		{name: "empty command", config: dockerExecCreateConfig{}},
		{name: "invalid environment", config: dockerExecCreateConfig{Env: []string{"BAD-KEY=value"}, Cmd: []string{"/bin/true"}}},
		{name: "relative working directory", config: dockerExecCreateConfig{WorkingDir: "relative", Cmd: []string{"/bin/true"}}},
		{name: "TTY console size", config: dockerExecCreateConfig{ConsoleSize: []uint{24, 80}, Cmd: []string{"/bin/true"}}},
		{name: "TTY detach keys", config: dockerExecCreateConfig{DetachKeys: "ctrl-p,ctrl-q", Cmd: []string{"/bin/true"}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := postDockerExecCreate(t, server, record.ID, test.config)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("exec create status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDockerEngineExecCreateRejectsStartOnlyDetachOption(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/"+record.ID+"/exec", strings.NewReader(`{"Cmd":["/bin/true"],"Detach":true}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("exec create with start-only Detach field status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerEngineExecRejectsMissingOrStoppedContainers(t *testing.T) {
	server := setupEngineAPITest(t)
	server.verifyExecPID = func(state.Record) error { return nil }
	missing := postDockerExecCreate(t, server, "missing-container", dockerExecCreateConfig{Cmd: []string{"/bin/true"}})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing container exec status=%d body=%s", missing.Code, missing.Body.String())
	}
	stopped, err := state.Create("stopped-exec-test", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := postDockerExecCreate(t, server, stopped.ID, dockerExecCreateConfig{Cmd: []string{"/bin/true"}})
	if response.Code != http.StatusConflict {
		t.Fatalf("stopped container exec status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerEngineExecRoutesValidateMethodsAndIdentifiers(t *testing.T) {
	server := setupEngineAPITest(t)
	for _, target := range []string{"/exec/bad-id/json", "/exec/bad-id/start", "/exec/id/unknown"} {
		response := engineAPIRequest(server, http.MethodGet, target)
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	response := engineAPIRequest(server, http.MethodGet, "/containers/no-such-container/exec")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("exec create wrong method status=%d body=%s", response.Code, response.Body.String())
	}
}

func decodeDockerExecFrames(t *testing.T, data []byte) (string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	reader := bytes.NewReader(data)
	for reader.Len() > 0 {
		var header [8]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			t.Fatalf("read Docker exec frame header: %v", err)
		}
		size := binary.BigEndian.Uint32(header[4:])
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			t.Fatalf("read Docker exec frame payload: %v", err)
		}
		switch header[0] {
		case 1:
			stdout.Write(payload)
		case 2:
			stderr.Write(payload)
		default:
			t.Fatalf("unexpected Docker exec stream type %d", header[0])
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			t.Fatalf("Docker exec frame reserved bytes = %v", header[1:4])
		}
	}
	return stdout.String(), stderr.String()
}

func TestDockerEngineExecInheritsContainerUser(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "exec-user-inherit", container.Options{User: "1001:100"})
	server.verifyExecPID = func(state.Record) error { return nil }
	response := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{Cmd: []string{"/bin/true"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("exec create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	session, ok := server.dockerExecSession(created.Id)
	if !ok {
		t.Fatal("exec session was not stored")
	}
	if session.Config.User != "1001:100" {
		t.Fatalf("inherited exec user=%q, want 1001:100", session.Config.User)
	}
}

