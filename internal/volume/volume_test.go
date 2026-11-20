package volume

import (
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
