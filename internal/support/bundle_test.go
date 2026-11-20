package support

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateProducesPrivateSanitizedArchive(t *testing.T) {
	output := filepath.Join(t.TempDir(), "support.zip")
	result, err := Create(output, false, Metadata{
		Version: "1.2.3", Commit: "abc123", BuildDate: "2026-11-20",
		Diagnostics: []Diagnostic{{Name: "cgroup v2", Status: "ok"}, {Name: "pid namespace", Status: "warn"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes == 0 || len(result.SHA256) != 64 {
		t.Fatalf("invalid result: %#v", result)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("archive mode = %o, want 600", info.Mode().Perm())
	}
	archive, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if len(archive.File) != 2 {
		t.Fatalf("entries = %d, want 2", len(archive.File))
	}
	var foundReport, foundReadme bool
	for _, entry := range archive.File {
		switch entry.Name {
		case "report.json":
			foundReport = true
			reader, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			var report Report
			if err := json.NewDecoder(reader).Decode(&report); err != nil {
				t.Fatal(err)
			}
			_ = reader.Close()
			if report.Boxr.Version != "1.2.3" || report.Diagnostics.OK != 1 || report.Diagnostics.Warn != 1 || len(report.Diagnostics.Checks) != 2 {
				t.Fatalf("unexpected report: %#v", report)
			}
			if report.Privacy.IncludesEnvironment || report.Privacy.IncludesCommandArgs ||
				report.Privacy.IncludesContainerLogs || report.Privacy.IncludesContainerState ||
				report.Privacy.IncludesFilesystemPaths || report.Privacy.IncludesConfiguration {
				t.Fatal("privacy exclusions not enforced")
			}
		case "README.txt":
			foundReadme = true
			reader, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			data := make([]byte, 2048)
			n, _ := reader.Read(data)
			_ = reader.Close()
			if !strings.Contains(string(data[:n]), "does not include environment variables") {
				t.Fatal("README missing privacy notice")
			}
		}
		if entry.Mode().Perm() != 0600 {
			t.Errorf("entry %s mode = %o, want 600", entry.Name, entry.Mode().Perm())
		}
	}
	if !foundReport || !foundReadme {
		t.Fatal("support archive is missing required entries")
	}
}

func TestCreateRequiresForceToReplace(t *testing.T) {
	output := filepath.Join(t.TempDir(), "support.zip")
	if _, err := Create(output, false, Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(output, false, Metadata{}); err == nil {
		t.Fatal("existing output was overwritten without force")
	}
	if _, err := Create(output, true, Metadata{}); err != nil {
		t.Fatalf("force replacement failed: %v", err)
	}
}

func TestCreateRejectsSymlinkAndInvalidDiagnostics(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "link.zip")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, output); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Create(output, true, Metadata{}); err == nil {
		t.Fatal("symlink destination was accepted")
	}
	if _, err := Create(filepath.Join(dir, "invalid.zip"), false, Metadata{Diagnostics: []Diagnostic{{Name: "../leak", Status: "ok"}}}); err == nil {
		t.Fatal("invalid diagnostic name was accepted")
	}
	if _, err := Create(filepath.Join(dir, "invalid-status.zip"), false, Metadata{Diagnostics: []Diagnostic{{Name: "check", Status: "secret"}}}); err == nil {
		t.Fatal("invalid diagnostic status was accepted")
	}
}
