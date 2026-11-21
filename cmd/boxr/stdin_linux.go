//go:build linux

package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/manit6752025/boxr/internal/state"
)

func openEngineContainerStdinBridge(id string, stdinOnce bool) (io.Reader, func(), error) {
	socketPath, err := state.EngineStdinSocketPath(id)
	if err != nil {
		return nil, nil, err
	}
	if len(socketPath) >= 100 {
		return nil, nil, fmt.Errorf("container stdin socket path is too long for a Unix-domain socket")
	}
	if info, statErr := os.Lstat(socketPath); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("refuse to replace non-socket stdin path %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, nil, fmt.Errorf("remove stale container stdin socket: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, nil, fmt.Errorf("inspect container stdin socket: %w", statErr)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for container stdin: %w", err)
	}
	socketInfo, err := os.Lstat(socketPath)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, nil, fmt.Errorf("container stdin socket was not created safely")
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, nil, fmt.Errorf("secure container stdin socket: %w", err)
	}

	reader, writer := io.Pipe()
	done := make(chan struct{})
	var closeWriter sync.Once
	finishInput := func() { closeWriter.Do(func() { _ = writer.Close() }) }
	go func() {
		defer close(done)
		defer finishInput()
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_, copyErr := io.Copy(writer, connection)
			_ = connection.Close()
			if stdinOnce {
				_ = listener.Close()
				return
			}
			if copyErr != nil {
				return
			}
		}
	}()

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			_ = listener.Close()
			_ = reader.Close()
			finishInput()
			<-done
			if current, statErr := os.Lstat(socketPath); statErr == nil &&
				current.Mode()&os.ModeSocket != 0 && os.SameFile(socketInfo, current) {
				_ = os.Remove(socketPath)
			}
		})
	}
	return reader, cleanup, nil
}
