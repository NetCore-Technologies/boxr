//go:build linux

package engineapi

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

func runDockerExecTTY(command *exec.Cmd, config dockerExecCreateConfig, stdin io.Reader, stdout io.Writer, onStart func(int), onTTY func(*os.File)) (int, error) {
	size := &pty.Winsize{Rows: 24, Cols: 80}
	if len(config.ConsoleSize) == 2 {
		size.Rows = uint16(config.ConsoleSize[0])
		size.Cols = uint16(config.ConsoleSize[1])
	}
	terminal, err := pty.StartWithSize(command, size)
	if err != nil {
		return 125, fmt.Errorf("start exec in a pseudo-terminal: %w", err)
	}
	if onStart != nil && command.Process != nil {
		onStart(command.Process.Pid)
	}
	if onTTY != nil {
		onTTY(terminal)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(terminal, stdin)
			// A terminal's canonical input mode interprets Ctrl-D as EOF.
			// Send it on both clean half-close and abrupt client disconnect so
			// interactive workloads cannot remain stuck waiting for input.
			_, _ = terminal.Write([]byte{4})
		}()
	}

	_, outputErr := io.Copy(stdout, terminal)
	if outputErr != nil && !errors.Is(outputErr, syscall.EIO) && !errors.Is(outputErr, os.ErrClosed) {
		// A failed attached stream must not leave an orphaned interactive
		// process waiting forever after its output consumer has disappeared.
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}
	waitErr := command.Wait()
	if onTTY != nil {
		onTTY(nil)
	}
	_ = terminal.Close()

	if outputErr != nil && !errors.Is(outputErr, syscall.EIO) && !errors.Is(outputErr, os.ErrClosed) {
		if waitErr == nil {
			return 125, fmt.Errorf("read exec terminal output: %w", outputErr)
		}
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			return exitError.ExitCode(), nil
		}
		return 125, fmt.Errorf("wait for terminal exec: %w", waitErr)
	}
	return 0, nil
}

func resizeDockerExecTTY(terminal *os.File, rows, columns uint16) error {
	if terminal == nil {
		return fmt.Errorf("exec terminal is not active")
	}
	return pty.Setsize(terminal, &pty.Winsize{Rows: rows, Cols: columns})
}

