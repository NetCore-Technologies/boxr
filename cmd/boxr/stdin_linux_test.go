//go:build linux

package main

import (
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestEngineContainerStdinBridgeAcceptsMultipleAttachSessions(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("stdin-bridge", []string{"/bin/cat"}, container.Options{OpenStdin: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, cleanup, err := openEngineContainerStdinBridge(record.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	socketPath, err := state.EngineStdinSocketPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, payload := range [][]byte{[]byte("first session\n"), []byte("second session\n")} {
		connection, err := net.DialTimeout("unix", socketPath, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Write(payload); err != nil {
			_ = connection.Close()
			t.Fatal(err)
		}
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(reader, got); err != nil {
			t.Fatalf("read stdin payload: %v", err)
		}
		if string(got) != string(payload) {
			t.Fatalf("stdin payload = %q, want %q", got, payload)
		}
	}
}

func TestEngineContainerStdinBridgeHonorsStdinOnce(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("stdin-once", []string{"/bin/cat"}, container.Options{
		OpenStdin: true, StdinOnce: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, cleanup, err := openEngineContainerStdinBridge(record.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	socketPath, err := state.EngineStdinSocketPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("only session\n")
	if _, err := connection.Write(payload); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("stdin-once payload = %q, want %q", got, payload)
	}
	socketPath, err = state.EngineStdinSocketPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("stdin-once socket should remain until bridge cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("stdin socket remains after cleanup: %v", err)
	}
}
