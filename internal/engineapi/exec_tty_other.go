//go:build !linux

package engineapi

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

func runDockerExecTTY(_ *exec.Cmd, _ dockerExecCreateConfig, _ io.Reader, _ io.Writer, _ func(int), _ func(*os.File)) (int, error) {
	return 125, fmt.Errorf("exec TTY support currently requires Linux")
}

func resizeDockerExecTTY(_ *os.File, _, _ uint16) error {
	return fmt.Errorf("exec TTY resizing currently requires Linux")
}
