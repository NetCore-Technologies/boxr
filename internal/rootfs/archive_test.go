package rootfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackAndExtractTarGzRoundTrip(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	if err := os.MkdirAll(filepath.Join(source, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "usr", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "etc", "os-release"), []byte("NAME=Alpine\nVERSION_ID=3.22\nPRETTY_NAME=\"Alpine Linux v3.22\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "usr", "bin", "sh"), []byte("shell-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin", filepath.Join(source, "bin")); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(parent, "rootfs.tar.gz")
	packed, err := PackTarGz(source, archive)
	if err != nil {
		t.Fatalf("PackTarGz(): %v", err)
	}
	if packed.Files != 2 || packed.Symlinks != 1 || packed.Bytes == 0 {
		t.Fatalf("unexpected packed manifest: %#v", packed)
	}

	destination := filepath.Join(parent, "extracted")
	extracted, err := ExtractTarGz(archive, destination, Limits{})
	if err != nil {
		t.Fatalf("ExtractTarGz(): %v", err)
	}
	if extracted.Files != packed.Files || extracted.Symlinks != packed.Symlinks || extracted.Bytes != packed.Bytes {
		t.Fatalf("packed/extracted manifests differ: %#v / %#v", packed, extracted)
	}
	link, err := os.Readlink(filepath.Join(destination, "bin"))
	if err != nil {
		t.Fatalf("read bin symlink: %v", err)
	}
	if link != "usr/bin" {
		t.Fatalf("absolute container symlink should be converted to safe relative link, got %q", link)
	}
	data, err := os.ReadFile(filepath.Join(destination, "etc", "os-release"))
	if err != nil || !strings.Contains(string(data), "Alpine") {
		t.Fatalf("extracted os-release = %q, err=%v", data, err)
	}
	inspected, err := Inspect(destination)
	if err != nil {
		t.Fatalf("Inspect(): %v", err)
	}
	if inspected.OSName != "Alpine Linux v3.22" || inspected.OSVersion != "3.22" {
		t.Fatalf("OS metadata = %#v", inspected)
	}
}

func TestExtractRejectsTraversalAndPreservesDestination(t *testing.T) {
	parent := t.TempDir()
	archive := filepath.Join(parent, "bad.tar.gz")
	writeTarGz(t, archive, []tarEntry{{name: "../escape", kind: tar.TypeReg, body: "owned"}})
	destination := filepath.Join(parent, "rootfs")
	if _, err := ExtractTarGz(archive, destination, Limits{}); err == nil {
		t.Fatal("path traversal archive was accepted")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination was created despite failed extraction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside destination: %v", err)
	}
}

func TestExtractRejectsEscapingAndAbsoluteSymlinksSafely(t *testing.T) {
	parent := t.TempDir()
	archive := filepath.Join(parent, "links.tar.gz")
	writeTarGz(t, archive, []tarEntry{
		{name: "bad", kind: tar.TypeSymlink, link: "../../../../etc/passwd"},
	})
	if _, err := ExtractTarGz(archive, filepath.Join(parent, "bad-root"), Limits{}); err == nil {
		t.Fatal("escaping symlink was accepted")
	}

	archive2 := filepath.Join(parent, "absolute.tar.gz")
	writeTarGz(t, archive2, []tarEntry{
		{name: "usr/bin", kind: tar.TypeDir},
		{name: "usr/bin/tool", kind: tar.TypeReg, body: "ok"},
		{name: "bin", kind: tar.TypeSymlink, link: "/usr/bin"},
	})
	destination := filepath.Join(parent, "good-root")
	if _, err := ExtractTarGz(archive2, destination, Limits{}); err != nil {
		t.Fatalf("safe absolute container symlink rejected: %v", err)
	}
	link, err := os.Readlink(filepath.Join(destination, "bin"))
	if err != nil || link != "usr/bin" {
		t.Fatalf("converted absolute symlink = %q, %v", link, err)
	}
}

func TestExtractRejectsArchiveWritesThroughSymlinkParents(t *testing.T) {
	parent := t.TempDir()
	archive := filepath.Join(parent, "symlink-parent.tar.gz")
	writeTarGz(t, archive, []tarEntry{
		{name: "safe", kind: tar.TypeDir},
		{name: "pivot", kind: tar.TypeSymlink, link: "safe"},
		{name: "pivot/owned", kind: tar.TypeReg, body: "must-not-be-written"},
	})
	destination := filepath.Join(parent, "rootfs")
	if _, err := ExtractTarGz(archive, destination, Limits{}); err == nil {
		t.Fatal("archive write through an earlier symlink was accepted")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed extraction published a destination: %v", err)
	}
}

func TestExtractRejectsSpecialFilesAndResourceLimitExceeded(t *testing.T) {
	parent := t.TempDir()
	archive := filepath.Join(parent, "special.tar.gz")
	writeTarGz(t, archive, []tarEntry{{name: "device", kind: tar.TypeChar}})
	if _, err := ExtractTarGz(archive, filepath.Join(parent, "special-root"), Limits{}); err == nil {
		t.Fatal("special file was accepted")
	}

	large := filepath.Join(parent, "large.tar.gz")
	writeTarGz(t, large, []tarEntry{{name: "payload", kind: tar.TypeReg, body: strings.Repeat("x", 128)}})
	if _, err := ExtractTarGz(large, filepath.Join(parent, "limited-root"), Limits{MaxFiles: 10, MaxBytes: 64}); err == nil {
		t.Fatal("oversized archive was accepted")
	}
	if _, err := ExtractTarGz(large, filepath.Join(parent, "entry-limited"), Limits{MaxFiles: 0, MaxBytes: 1}); err == nil {
		t.Fatal("oversized bytes were accepted")
	}
}

func TestExtractRequiresNewDestination(t *testing.T) {
	parent := t.TempDir()
	archive := filepath.Join(parent, "small.tar.gz")
	writeTarGz(t, archive, []tarEntry{{name: "file", kind: tar.TypeReg, body: "ok"}})
	destination := filepath.Join(parent, "existing")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "keep"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTarGz(archive, destination, Limits{}); err == nil {
		t.Fatal("existing destination was accepted")
	}
	if data, err := os.ReadFile(filepath.Join(destination, "keep")); err != nil || string(data) != "keep" {
		t.Fatalf("existing destination was modified: %q, %v", data, err)
	}
}

func TestPackRejectsDestinationInsideRootfs(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := PackTarGz(root, filepath.Join(root, "nested.tar.gz")); err == nil {
		t.Fatal("archive inside rootfs was accepted")
	}
}

type tarEntry struct {
	name string
	kind byte
	body string
	link string
}

func writeTarGz(t *testing.T, name string, entries []tarEntry) {
	t.Helper()
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0644, Linkname: entry.link, Size: int64(len(entry.body))}
		if entry.kind == tar.TypeDir {
			header.Mode = 0755
			header.Size = 0
		}
		if entry.kind == tar.TypeSymlink {
			header.Mode = 0777
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			if _, err := writer.Write(bytes.NewBufferString(entry.body).Bytes()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPackTarGzIsByteReproducibleAcrossDifferentTimestamps(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	for _, root := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "etc", "boxr.conf"), []byte("mode=stable\n"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("boxr.conf", filepath.Join(root, "etc", "current")); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC)
	newer := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(first, "etc", "boxr.conf"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(first, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(second, "etc", "boxr.conf"), newer, newer); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(second, newer, newer); err != nil {
		t.Fatal(err)
	}
	archiveA := filepath.Join(parent, "first.tar.gz")
	archiveB := filepath.Join(parent, "second.tar.gz")
	if _, err := PackTarGz(first, archiveA); err != nil {
		t.Fatal(err)
	}
	if _, err := PackTarGz(second, archiveB); err != nil {
		t.Fatal(err)
	}
	dataA, err := os.ReadFile(archiveA)
	if err != nil {
		t.Fatal(err)
	}
	dataB, err := os.ReadFile(archiveB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dataA, dataB) {
		t.Fatal("equivalent rootfs trees produced different archive bytes")
	}
}
