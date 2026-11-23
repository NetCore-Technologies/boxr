//go:build !linux

package engineapi

import "fmt"

func resizeDockerContainerTTY(_ string, _, _ uint16) error {
	return fmt.Errorf("container TTY resize currently requires Linux")
}
