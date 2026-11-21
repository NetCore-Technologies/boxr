//go:build !linux

package engineapi

import (
	"fmt"
	"net"
)

func openDockerContainerStdin(_ string) (net.Conn, error) {
	return nil, fmt.Errorf("container stdin attachment currently requires Linux")
}
