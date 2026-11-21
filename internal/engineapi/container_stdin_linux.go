//go:build linux

package engineapi

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/manit6752025/boxr/internal/state"
)

func openDockerContainerStdin(id string) (net.Conn, error) {
	path, err := state.EngineStdinSocketPath(id)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("container stdin is not available: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("container stdin endpoint is not a private Unix socket")
	}
	connection, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to container stdin: %w", err)
	}
	return connection, nil
}
