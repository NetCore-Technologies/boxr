//go:build linux

package engineapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestDockerExecTTYResizeChangesPTYSize(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open pseudo-terminal: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if err := resizeDockerExecTTY(master, 41, 121); err != nil {
		t.Fatalf("resize pseudo-terminal: %v", err)
	}
	size, err := pty.Getsize(master)
	if err != nil {
		t.Fatalf("read pseudo-terminal size: %v", err)
	}
	if size.Rows != 41 || size.Cols != 121 {
		t.Fatalf("pseudo-terminal size = %dx%d, want 41x121", size.Rows, size.Cols)
	}
}

func TestDockerExecTTYRunnerConnectsStdinAndMergesOutput(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", `read value; printf 'received:%s\n' "$value"`)
	var output bytes.Buffer
	exitCode, err := runDockerExecTTY(command, dockerExecCreateConfig{
		Tty: true, ConsoleSize: []uint{32, 100},
	}, strings.NewReader("interactive-input
"), &output, nil, nil)
	if err != nil {
		t.Fatalf("run TTY exec: %v; output=%q", err, output.String())
	}
	if exitCode != 0 {
		t.Fatalf("TTY exec exit code=%d; output=%q", exitCode, output.String())
	}
	if !strings.Contains(output.String(), "received:interactive-input") {
		t.Fatalf("TTY output %q does not contain command output", output.String())
	}
}

func TestDockerExecResizeUpdatesRunningSessionPTY(t *testing.T) {
	server := setupEngineAPITest(t)
	server.verifyExecPID = func(state.Record) error { return nil }
	record := createEngineAPITestContainer(t, "exec-pty-resize", container.Options{})
	createdResponse := postDockerExecCreate(t, server, record.ID, dockerExecCreateConfig{
		Tty: true, Cmd: []string{"/bin/sh"},
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
	started := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	var master *os.File
	dockerExecRunner = func(_ context.Context, _ string, _ dockerExecCreateConfig, _ io.Reader, _, _ io.Writer, onStart func(int), onTTY func(*os.File)) (int, error) {
		var slave *os.File
		var err error
		master, slave, err = pty.Open()
		if err != nil {
			return 125, err
		}
		_ = slave.Close()
		onStart(8765)
		onTTY(master)
		close(started)
		<-release
		onTTY(nil)
		_ = master.Close()
		return 0, nil
	}

	request := httptest.NewRequest(http.MethodPost, "/v1.45/exec/"+created.Id+"/start", strings.NewReader(`{"Tty":true}`))
	response := httptest.NewRecorder()
	go func() {
		server.ServeHTTP(response, request)
		close(handlerDone)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("exec runner did not publish its active PTY")
	}
	resize := engineAPIRequest(server, http.MethodPost, "/exec/"+created.Id+"/resize?h=41&w=121")
	if resize.Code != http.StatusOK {
		close(release)
		t.Fatalf("active exec resize status=%d body=%s", resize.Code, resize.Body.String())
	}
	size, err := pty.Getsize(master)
	if err != nil {
		close(release)
		t.Fatalf("read resized PTY dimensions: %v", err)
	}
	if size.Rows != 41 || size.Cols != 121 {
		close(release)
		t.Fatalf("active PTY size = %dx%d, want 41x121", size.Rows, size.Cols)
	}
	close(release)
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("exec start handler did not finish")
	}
}
