package rootfs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAuditFindsUnsafePermissionsAndEscapingSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	setuidPath := filepath.Join(root, "bin", "setuid-tool")
	if err := os.WriteFile(setuidPath, []byte("test"), 04755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(setuidPath, 04755); err != nil {
		t.Fatal(err)
	}
	worldWritable := filepath.Join(root, "world.txt")
	if err := os.WriteFile(worldWritable, []byte("shared"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worldWritable, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(root, "bin", "escape")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	report, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, finding := range report.Findings {
		codes[finding.Code] = true
	}
	setuidInfo, err := os.Lstat(setuidPath)
	if err != nil {
		t.Fatal(err)
	}
	wantCodes := []string{"world-writable", "escaping-symlink"}
	if setuidInfo.Mode()&os.ModeSetuid != 0 {
		wantCodes = append(wantCodes, "setuid")
	}
	for _, code := range wantCodes {
		if !codes[code] {
			t.Errorf("missing %s finding: %+v", code, report.Findings)
		}
	}
	if !report.HasFindings() || report.EntriesScanned != 4 {
		t.Fatalf("unexpected audit summary: %+v", report)
	}
}

func TestAuditDoesNotFollowSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	externalFile := filepath.Join(external, "outside")
	if err := os.WriteFile(externalFile, []byte("external"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(externalFile, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "external")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	report, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.EntriesScanned != 1 {
		t.Fatalf("audit followed symlink target: scanned %d entries", report.EntriesScanned)
	}
	for _, finding := range report.Findings {
		if finding.Path == "external/outside" {
			t.Fatal("audit followed a symlink outside the rootfs")
		}
	}
}

func TestAuditReportsStickyWorldWritableDirectoryAsInfo(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "tmp")
	if err := os.Mkdir(shared, 01777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 01777); err != nil {
		t.Fatal(err)
	}
	report, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Skip("filesystem does not preserve sticky directory mode")
	}
	if report.InfoCount != 1 || report.WarningCount != 0 || report.HasFindings() {
		t.Fatalf("sticky directory should be informational: %+v", report)
	}
}

func TestAuditRejectsSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink privileges are environment dependent")
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := Audit(link); err == nil {
		t.Fatal("audit accepted symlink root")
	}
}

func TestAuditWithLimitFailsInsteadOfReturningPartialResults(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AuditWithLimit(root, 2); err == nil || !strings.Contains(err.Error(), "2-entry limit") {
		t.Fatalf("expected explicit entry-limit error, got %v", err)
	}
	if _, err := AuditWithLimit(root, 0); err == nil {
		t.Fatal("zero audit entry limit unexpectedly succeeded")
	}
}
