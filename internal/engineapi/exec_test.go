package engineapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	dockerExecRunner = func(_ context.Context, containerID string, config dockerExecCreateConfig, _ io.Reader, stdout, stderr io.Writer, onStart func(int), _ func(*os.File)) (int, error) {
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

func TestDockerEngineExecInteractiveTTYUsesHijackedRawStream(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	createdResponse := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{
		AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: true,
		ConsoleSize: []uint{25, 80}, Cmd: []string{"/bin/cat"},
	})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("exec create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	originalRunner := dockerExecRunner
	t.Cleanup(func() { dockerExecRunner = originalRunner })
	dockerExecRunner = func(_ context.Context, containerID string, config dockerExecCreateConfig, stdin io.Reader, stdout, stderr io.Writer, onStart func(int), _ func(*os.File)) (int, error) {
		if containerID != record.ID || !config.Tty || !config.AttachStdin || len(config.ConsoleSize) != 2 {
			return 125, fmt.Errorf("interactive exec configuration was not preserved")
		}
		onStart(9876)
		input, err := io.ReadAll(stdin)
		if err != nil {
			return 125, err
		}
		if _, err := fmt.Fprintf(stdout, "stdout:%s", input); err != nil {
			return 125, err
		}
		if _, err := io.WriteString(stderr, "stderr:combined"); err != nil {
			return 125, err
		}
		return 0, nil
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
	body, err := json.Marshal(dockerExecStartConfig{Tty: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection,
		"POST /v1.45/exec/%s/start HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		created.Id, target.Host, len(body), body); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, "101 UPGRADED") {
		t.Fatalf("exec upgrade response = %q", statusLine)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := connection.Write([]byte("interactive input\n")); err != nil {
		t.Fatal(err)
	}
	tcpConnection, ok := connection.(*net.TCPConn)
	if !ok {
		t.Fatal("expected TCP connection for half-close")
	}
	if err := tcpConnection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(output), "stdout:interactive input\nstderr:combined"; got != want {
		t.Fatalf("TTY output = %q, want %q (TTY output must be unframed and merge stdout/stderr)", got, want)
	}

	inspect := engineAPIRequest(server, http.MethodGet, "/exec/"+created.Id+"/json")
	var result struct {
		Running       bool
		ExitCode      *int
		Pid           int
		ProcessConfig struct {
			TTY bool `json:"tty"`
		}
		OpenStdin bool
	}
	if err := json.Unmarshal(inspect.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Running || result.ExitCode == nil || *result.ExitCode != 0 || result.Pid != 9876 ||
		!result.ProcessConfig.TTY || !result.OpenStdin {
		t.Fatalf("interactive exec inspect result = %+v", result)
	}
}

func TestDockerEngineExecAttachStdinRequiresHijack(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	response := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{
		AttachStdin: true, Cmd: []string{"/bin/cat"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("exec create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	start := engineAPIRequest(server, http.MethodPost, "/exec/"+created.Id+"/start")
	if start.Code != http.StatusBadRequest {
		t.Fatalf("exec start without hijack status=%d body=%s", start.Code, start.Body.String())
	}
}

func TestDockerEngineExecResizeValidatesDimensionsAndSessionState(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	response := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{
		Tty: true, Cmd: []string{"/bin/sh"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("exec create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	invalid := engineAPIRequest(server, http.MethodPost, "/exec/"+created.Id+"/resize?h=0&w=80")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid exec resize status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	notRunning := engineAPIRequest(server, http.MethodPost, "/exec/"+created.Id+"/resize?h=24&w=80")
	if notRunning.Code != http.StatusConflict {
		t.Fatalf("inactive exec resize status=%d body=%s", notRunning.Code, notRunning.Body.String())
	}
}

func TestDockerEngineExecSessionCanBeDeletedAfterCompletion(t *testing.T) {
	server := setupEngineAPITest(t)
	record := execAPITestContainer(t, server)
	response := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{Cmd: []string{"/bin/true"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("exec create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	originalRunner := dockerExecRunner
	t.Cleanup(func() { dockerExecRunner = originalRunner })
	dockerExecRunner = func(_ context.Context, _ string, _ dockerExecCreateConfig, _ io.Reader, _, _ io.Writer, onStart func(int), _ func(*os.File)) (int, error) {
		onStart(7654)
		return 0, nil
	}
	start := httptest.NewRecorder()
	server.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/exec/"+created.Id+"/start", strings.NewReader("{}")))
	if start.Code != http.StatusOK {
		t.Fatalf("exec start status=%d body=%s", start.Code, start.Body.String())
	}
	deleted := engineAPIRequest(server, http.MethodDelete, "/exec/"+created.Id)
	if deleted.Code != http.StatusOK {
		t.Fatalf("exec delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	inspect := engineAPIRequest(server, http.MethodGet, "/exec/"+created.Id+"/json")
	if inspect.Code != http.StatusNotFound {
		t.Fatalf("deleted exec inspect status=%d body=%s", inspect.Code, inspect.Body.String())
	}
}

func TestDockerEngineExecRunningSessionCannotBeDeleted(t *testing.T) {
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
	dockerExecRunner = func(_ context.Context, _ string, _ dockerExecCreateConfig, _ io.Reader, _, _ io.Writer, onStart func(int), _ func(*os.File)) (int, error) {
		onStart(8765)
		close(started)
		<-release
		return 0, nil
	}
	startDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/exec/"+created.Id+"/start", strings.NewReader(`{"Detach":true}`))
		server.ServeHTTP(httptest.NewRecorder(), request)
		close(startDone)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("exec runner did not start")
	}
	deleted := engineAPIRequest(server, http.MethodDelete, "/exec/"+created.Id)
	if deleted.Code != http.StatusConflict {
		close(release)
		t.Fatalf("running exec delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	close(release)
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("detached exec start endpoint did not return")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session, exists := server.dockerExecSession(created.Id)
		if !exists {
			t.Fatal("exec session disappeared before deletion")
		}
		session.mu.Lock()
		running := session.Running
		session.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached exec runner did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	deleted = engineAPIRequest(server, http.MethodDelete, "/exec/"+created.Id)
	if deleted.Code != http.StatusOK {
		t.Fatalf("completed exec delete status=%d body=%s", deleted.Code, deleted.Body.String())
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
	dockerExecRunner = func(_ context.Context, _ string, _ dockerExecCreateConfig, _ io.Reader, _, _ io.Writer, onStart func(int), _ func(*os.File)) (int, error) {
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

		{name: "privileged", config: dockerExecCreateConfig{Privileged: true, Cmd: []string{"/bin/true"}}},
		{name: "invalid user", config: dockerExecCreateConfig{User: "bad user", Cmd: []string{"/bin/true"}}},
		{name: "empty command", config: dockerExecCreateConfig{}},
		{name: "invalid environment", config: dockerExecCreateConfig{Env: []string{"BAD-KEY=value"}, Cmd: []string{"/bin/true"}}},
		{name: "relative working directory", config: dockerExecCreateConfig{WorkingDir: "relative", Cmd: []string{"/bin/true"}}},
		{name: "console size without TTY", config: dockerExecCreateConfig{ConsoleSize: []uint{24, 80}, Cmd: []string{"/bin/true"}}},
		{name: "invalid TTY console size", config: dockerExecCreateConfig{Tty: true, ConsoleSize: []uint{24}, Cmd: []string{"/bin/true"}}},
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

func TestDockerEngineExecUpgradeStreamsStdinWithAndWithoutTTY(t *testing.T) {
	for _, test := range []struct {
		name string
		tty  bool
	}{
		{name: "multiplexed non-TTY", tty: false},
		{name: "raw PTY", tty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := setupEngineAPITest(t)
			record := execAPITestContainer(t, server)
			create := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{
				AttachStdin: true, AttachStdout: true, AttachStderr: true,
				Tty: test.tty, Cmd: []string{"/bin/cat"},
			})
			if create.Code != http.StatusCreated {
				t.Fatalf("exec create status=%d body=%s", create.Code, create.Body.String())
			}
			var created struct{ ID string }
			if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}

			originalRunner := dockerExecRunner
			t.Cleanup(func() { dockerExecRunner = originalRunner })
			var received string
			dockerExecRunner = func(_ context.Context, _ string, _ dockerExecCreateConfig, stdin io.Reader, stdout, stderr io.Writer, _ func(int), _ func(*os.File)) (int, error) {
				payload, err := io.ReadAll(stdin)
				if err != nil {
					return 125, err
				}
				received = string(payload)
				if _, err := io.WriteString(stdout, "stdout:"+received); err != nil {
					return 125, err
				}
				if _, err := io.WriteString(stderr, "stderr"); err != nil {
					return 125, err
				}
				return 0, nil
			}

			httpServer := httptest.NewServer(server)
			defer httpServer.Close()
			target, err := url.Parse(httpServer.URL)
			if err != nil {
				t.Fatal(err)
			}
			connection, err := net.DialTimeout("tcp", target.Host, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			tcpConnection := connection.(*net.TCPConn)
			defer connection.Close()
			body := `{"Tty":` + strconv.FormatBool(test.tty) + `}`
			requestPath := "/v1.45/exec/" + created.ID + "/start"
			newline := string([]byte{13, 10})
			request := strings.Join([]string{
				"POST " + requestPath + " HTTP/1.1",
				"Host: " + target.Host,
				"Connection: Upgrade",
				"Upgrade: tcp",
				"Content-Type: application/json",
				"Content-Length: " + strconv.Itoa(len(body)),
				"",
				body,
			}, newline)
			if _, err := io.WriteString(connection, request); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			statusLine, err := reader.ReadString(10)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(statusLine, "101 UPGRADED") {
				t.Fatalf("Docker exec upgrade status = %q", statusLine)
			}
			for {
				line, err := reader.ReadString(10)
				if err != nil {
					t.Fatal(err)
				}
				if line == newline || line == string([]byte{10}) {
					break
				}
			}
			payload := "interactive input" + string([]byte{10})
			if _, err := io.WriteString(connection, payload); err != nil {
				t.Fatal(err)
			}
			if err := tcpConnection.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			output, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if received != payload {
				t.Fatalf("exec received stdin %q, want %q", received, payload)
			}
			if test.tty {
				if string(output) != "stdout:"+payload+"stderr" {
					t.Fatalf("TTY exec raw output = %q", output)
				}
			} else {
				stdout, stderr := decodeDockerExecFrames(t, output)
				if stdout != "stdout:"+payload || stderr != "stderr" {
					t.Fatalf("exec streams stdout=%q stderr=%q", stdout, stderr)
				}
			}
		})
	}
}
