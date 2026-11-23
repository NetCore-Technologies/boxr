//go:build linux

package engineapi

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/manit6752025/boxr/internal/state"
)

func resizeDockerContainerTTY(id string, rows, columns uint16) error {
	if rows == 0 || columns == 0 {
		return fmt.Errorf("container TTY dimensions must be positive")
	}
	path, err := state.EngineTTYSocketPath(id)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("container TTY resize endpoint is not available: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("container TTY resize endpoint is not a private Unix socket")
	}
	connection, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to container TTY resize endpoint: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	var payload [4]byte
	binary.BigEndian.PutUint16(payload[0:2], rows)
	binary.BigEndian.PutUint16(payload[2:4], columns)
	if _, err := connection.Write(payload[:]); err != nil {
		return fmt.Errorf("send container TTY dimensions: %w", err)
	}
	var response [1]byte
	if _, err := io.ReadFull(connection, response[:]); err != nil {
		return fmt.Errorf("read container TTY resize acknowledgement: %w", err)
	}
	if response[0] != 0 {
		return fmt.Errorf("container TTY resize was rejected")
	}
	return nil
}
