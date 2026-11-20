//go:build linux

package rootfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAuditReportsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "worker.pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("FIFO creation unavailable: %v", err)
	}
	report, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Code == "special-file" && finding.Path == "worker.pipe" && finding.Severity == "high" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing special-file audit finding: %+v", report.Findings)
	}
	if info, err := os.Lstat(fifo); err != nil || !info.Mode().IsRegular() && info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO test fixture unexpectedly changed: info=%v err=%v", info, err)
	}
}
