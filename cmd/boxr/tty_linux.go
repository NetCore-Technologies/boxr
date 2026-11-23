//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/state"
)

// openEngineContainerTTYResizeBridge publishes a private Unix socket for the
// daemon. Each request carries one big-endian rows/columns pair and receives
// a one-byte success/failure response.
func openEngineContainerTTYResizeBridge(id string, resize func(rows, columns uint16) error) (func(), error) {
	if resize == nil {
		return nil, fmt.Errorf("container TTY resize callback is required")
	}
	socketPath, err := state.EngineTTYSocketPath(id)
	if err != nil {
		return nil, err
	}
	if len(socketPath) >= 100 {
		return nil, fmt.Errorf("container TTY socket path exceeds the Unix-domain socket limit")
	}
	if info, statErr := os.Lstat(socketPath); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refuse to replace non-socket TTY path %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale container TTY socket: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect container TTY socket: %w", statErr)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen for container TTY resize: %w", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		return nil, fmt.Errorf("container TTY listener is not a Unix socket")
	}
	unixListener.SetUnlinkOnClose(false)
	socketInfo, err := os.Lstat(socketPath)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("container TTY socket was not created safely")
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("secure container TTY socket: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				var payload [4]byte
				if _, err := io.ReadFull(connection, payload[:]); err != nil {
					_, _ = connection.Write([]byte{1})
					return
				}
				rows := binary.BigEndian.Uint16(payload[0:2])
				columns := binary.BigEndian.Uint16(payload[2:4])
				if rows == 0 || columns == 0 {
					_, _ = connection.Write([]byte{1})
					return
				}
				status := byte(0)
				if err := resize(rows, columns); err != nil {
					status = 1
				}
				_, _ = connection.Write([]byte{status})
			}()
		}
	}()
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			_ = listener.Close()
			<-done
			if current, statErr := os.Lstat(socketPath); statErr == nil &&
				current.Mode()&os.ModeSocket != 0 && os.SameFile(socketInfo, current) {
				_ = os.Remove(socketPath)
			}
		})
	}
	return cleanup, nil
}
