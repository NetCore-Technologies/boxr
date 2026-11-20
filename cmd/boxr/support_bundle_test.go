package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSupportBundleCommandProducesPrivateSanitizedArchive(t *testing.T) {
	output := filepath.Join(t.TempDir(), "boxr-support.zip")
	t.Setenv("BOXR_SUPPORT_BUNDLE_SECRET", "must-not-appear-in-bundle")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"support-bundle", "--output", output, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("support-bundle returned %d: %s", code, stderr.String())
	}

	var result struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode support-bundle result: %v", err)
	}
	if result.Path != output || result.Bytes <= 0 || len(result.SHA256) != 64 {
		t.Fatalf("unexpected support-bundle result: %#v", result)
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
		t.Fatalf("archive has %d entries, want report and README", len(archive.File))
	}

	var combined strings.Builder
	for _, entry := range archive.File {
		if entry.Mode().Perm() != 0600 {
			t.Errorf("entry %s mode = %o, want 600", entry.Name, entry.Mode().Perm())
		}
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		combined.Write(data)
	}
	if strings.Contains(combined.String(), "must-not-appear-in-bundle") {
		t.Fatal("support archive leaked an environment variable")
	}
	if !strings.Contains(combined.String(), "does not include environment variables") {
		t.Fatal("support archive is missing its privacy notice")
	}

	if code := run([]string{"support-bundle", "--output", output}, &stdout, &stderr); code != 1 {
		t.Fatalf("existing output returned %d, want 1", code)
	}
	if code := run([]string{"support-bundle", "--output", output, "--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("support-bundle --force returned %d: %s", code, stderr.String())
	}
}

func TestSupportBundleCommandRequiresOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"support-bundle"}, &stdout, &stderr); code != 2 {
		t.Fatalf("support-bundle without output returned %d, want 2", code)
	}
}
