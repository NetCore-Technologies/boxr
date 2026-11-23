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

func TestEngineContainerTTYResizeBridgeAppliesWindowSize(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("tty-resize-bridge", []string{"/bin/sh"}, container.Options{TTY: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	applied := make(chan [2]uint16, 1)
	cleanup, err := openEngineContainerTTYResizeBridge(record.ID, func(rows, columns uint16) error {
		applied <- [2]uint16{rows, columns}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	path, err := state.EngineTTYSocketPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte{0, 40, 0, 120}); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	var response [1]byte
	if _, err := io.ReadFull(connection, response[:]); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	_ = connection.Close()
	select {
	case got := <-applied:
		if response[0] != 0 || got[0] != 40 || got[1] != 120 {
			t.Fatalf("resize response=%d dimensions=%dx%d, want success 40x120", response[0], got[0], got[1])
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not apply the terminal dimensions")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("TTY socket permissions should be private: info=%v err=%v", info, err)
	}
}

func TestEngineContainerTTYResizeBridgeRejectsZeroDimensions(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("tty-resize-invalid", []string{"/bin/sh"}, container.Options{TTY: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	cleanup, err := openEngineContainerTTYResizeBridge(record.ID, func(_, _ uint16) error {
		called <- struct{}{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	path, err := state.EngineTTYSocketPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = connection.Write([]byte{0, 0, 0, 80})
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	var response [1]byte
	if _, err := io.ReadFull(connection, response[:]); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	_ = connection.Close()
	if response[0] == 0 {
		t.Fatal("zero-sized resize request unexpectedly succeeded")
	}
	select {
	case <-called:
		t.Fatal("zero-sized request reached the PTY resize callback")
	default:
	}
}
