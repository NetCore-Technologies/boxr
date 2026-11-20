package volume

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func useTempData(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	t.Setenv("BOXR_DATA_DIR", path)
	return path
}

func TestExportArchiveDigestRoundTripAndNoClobber(t *testing.T) {
	useTempData(t)
	created, err := Create("backup-source", map[string]string{"purpose": "backup"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(created.Path, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "nested", "data.bin"), []byte("important volume data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/data.bin", filepath.Join(created.Path, "current")); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	archive, err := ExportArchive(created.Name, archivePath)
	if err != nil {
		t.Fatalf("ExportArchive: %v", err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if archive.Volume != created.Name || archive.Path != archivePath ||
		archive.Digest != "sha256:"+hex.EncodeToString(digest[:]) || archive.SizeBytes != int64(len(data)) {
		t.Fatalf("archive metadata does not match the exported file: %+v", archive)
	}
	if archive.Manifest.Files != 1 || archive.Manifest.Symlinks != 1 {
		t.Fatalf("archive manifest = %+v", archive.Manifest)
	}

	restored, manifest, err := ImportArchive("backup-restored", archivePath, nil)
	if err != nil {
		t.Fatalf("ImportArchive: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(restored.Path, "nested", "data.bin"))
	if err != nil || string(contents) != "important volume data" {
		t.Fatalf("restored data = %q, %v", contents, err)
	}
	link, err := os.Readlink(filepath.Join(restored.Path, "current"))
	if err != nil || link != "nested/data.bin" {
		t.Fatalf("restored symlink = %q, %v", link, err)
	}
	if manifest.Files != archive.Manifest.Files || manifest.Symlinks != archive.Manifest.Symlinks {
		t.Fatalf("restored manifest = %+v, want %+v", manifest, archive.Manifest)
	}

	if _, err := ExportArchive(created.Name, archivePath); err == nil {
		t.Fatal("ExportArchive unexpectedly replaced an existing backup")
	}
	after, err := os.ReadFile(archivePath)
	if err != nil || string(after) != string(data) {
		t.Fatalf("existing backup changed after rejected overwrite: %v", err)
	}
}

func TestExportArchiveRejectsDestinationInsideSourceVolume(t *testing.T) {
	useTempData(t)
	created, err := Create("no-recursion", nil)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(created.Path, "backup.tar.gz")
	if _, err := ExportArchive(created.Name, destination); err == nil {
		t.Fatal("archive destination inside the source volume unexpectedly accepted")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("rejected export left an archive in the source volume: %v", err)
	}
}

func TestCreateGetListAndRemoveVolume(t *testing.T) {
	base := useTempData(t)
	created, err := Create("cache-data", map[string]string{"purpose": "tests"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "cache-data" || created.Labels["purpose"] != "tests" {
		t.Fatalf("created volume = %#v", created)
	}
	if info, err := os.Stat(created.Path); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("volume directory mode: %v %v", info, err)
	}
	meta, err := os.Stat(filepath.Join(base, "volume-metadata", "cache-data.json"))
	if err != nil || meta.Mode().Perm() != 0600 {
		t.Fatalf("metadata mode: %v %v", meta, err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "cache.bin"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Get("cache-data")
	if err != nil || got.Path != created.Path {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	items, err := List()
	if err != nil || len(items) != 1 || items[0].Name != "cache-data" {
		t.Fatalf("List() = %#v, %v", items, err)
	}
	if err := Remove("cache-data"); err != nil {
		t.Fatal(err)
	}
	if _, err := Get("cache-data"); err == nil {
		t.Fatal("removed volume still exists")
	}
}

func TestVolumeRejectsDuplicateNamesAndPathTraversal(t *testing.T) {
	useTempData(t)
	if _, err := Create("volume1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Create("volume1", nil); err == nil {
		t.Fatal("duplicate volume was accepted")
	}
	for _, name := range []string{"../escape", "a/b", "with space", ".", "-bad", ""} {
		if _, err := Create(name, nil); err == nil {
			t.Errorf("invalid volume name %q accepted", name)
		}
	}
}

func TestGetRejectsSymlinkedVolumeData(t *testing.T) {
	base := useTempData(t)
	created, err := Create("linked", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(created.Path); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, created.Path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Get("linked"); err == nil {
		t.Fatal("symlinked volume path accepted")
	}
	if err := Remove("linked"); err == nil {
		t.Fatal("symlinked volume removal accepted")
	}
	if _, err := os.Stat(filepath.Join(base, "volume-metadata", "linked.json")); err != nil {
		t.Fatalf("metadata unexpectedly removed: %v", err)
	}
}

func TestCreateRejectsInvalidLabels(t *testing.T) {
	useTempData(t)
	for _, labels := range []map[string]string{{"": "value"}, {"bad=key": "value"}, {"key": "has\nnewline"}} {
		if _, err := Create("bad-label", labels); err == nil {
			t.Errorf("invalid labels accepted: %#v", labels)
		}
	}
}
