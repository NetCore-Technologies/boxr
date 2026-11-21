//go:build linux

package engineapi

import (
	"testing"

	"github.com/creack/pty"
)

func TestDockerExecTTYResizeChangesPTYSize(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open pseudo-terminal: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if err := resizeDockerExecTTY(master, 41, 121); err != nil {
		t.Fatalf("resize pseudo-terminal: %v", err)
	}
	size, err := pty.Getsize(master)
	if err != nil {
		t.Fatalf("read pseudo-terminal size: %v", err)
	}
	if size.Rows != 41 || size.Cols != 121 {
		t.Fatalf("pseudo-terminal size = %dx%d, want 41x121", size.Rows, size.Cols)
	}
}
