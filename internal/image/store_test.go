package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/rootfs"
)

func useStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(dir, "images"))
	return dir
}

func newRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "hello"), []byte("hello image\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestImportListInspectUnpackRemove(t *testing.T) {
	useStore(t)
	root := newRoot(t)
	created, err := ImportRootFS(root, "demo:test")
	if err != nil {
		t.Fatalf("ImportRootFS: %v", err)
	}
	if created.Reference != "demo:test" || !strings.HasPrefix(created.Digest, "sha256:") {
		t.Fatalf("unexpected image: %+v", created)
	}
	items, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Reference != "demo:test" {
		t.Fatalf("unexpected images: %+v", items)
	}
	inspected, err := Inspect("demo:test")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspected.Digest != created.Digest {
		t.Fatalf("digest mismatch: %s != %s", inspected.Digest, created.Digest)
	}
	destination := filepath.Join(t.TempDir(), "unpacked")
	manifest, err := Unpack("demo:test", destination)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if manifest.Files != 1 {
		t.Fatalf("unexpected unpack manifest: %+v", manifest)
	}
	data, err := os.ReadFile(filepath.Join(destination, "bin", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello image\n" {
		t.Fatalf("unexpected content %q", data)
	}
	if err := Remove("demo:test"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Inspect("demo:test"); err == nil {
		t.Fatal("Inspect after remove unexpectedly succeeded")
	}
}

func TestImportReplacesTagWithoutDuplicatingIndex(t *testing.T) {
	useStore(t)
	first := newRoot(t)
	if _, err := ImportRootFS(first, "demo:latest"); err != nil {
		t.Fatal(err)
	}
	second := newRoot(t)
	if err := os.WriteFile(filepath.Join(second, "bin", "hello"), []byte("updated\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(second, "demo:latest"); err != nil {
		t.Fatal(err)
	}
	items, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one tag, got %d", len(items))
	}
	if items[0].Digest == "" {
		t.Fatal("tag has no digest")
	}
}

func TestValidateReference(t *testing.T) {
	valid := []string{"alpine:3.20", "org/team/image:latest", "demo_1:v2.0.1", "ghcr.io/team/image:tag", "localhost:5000/team/image:v1"}
	for _, ref := range valid {
		if err := ValidateReference(ref); err != nil {
			t.Errorf("%q rejected: %v", ref, err)
		}
	}
	invalid := []string{"", "alpine", "../escape:tag", "x/../../y:tag", "image:tag/", "image:tag:again", "image::tag", "image://bad:tag"}
	for _, ref := range invalid {
		if err := ValidateReference(ref); err == nil {
			t.Errorf("%q unexpectedly accepted", ref)
		}
	}
}

func TestDetectsCorruptBlob(t *testing.T) {
	base := useStore(t)
	created, err := ImportRootFS(newRoot(t), "demo:corrupt")
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "images")
	indexData, err := os.ReadFile(filepath.Join(store, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(store, "blobs", "sha256", strings.TrimPrefix(index.Manifests[0].Digest, "sha256:"))
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	layerPath := filepath.Join(store, "blobs", "sha256", strings.TrimPrefix(manifest.Layers[0].Digest, "sha256:"))
	if err := os.Chmod(layerPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layerPath, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect("demo:corrupt"); err == nil {
		t.Fatal("corrupt layer was accepted")
	}
	if created.Digest == "" {
		t.Fatal("empty digest")
	}
}

func TestImportRejectsRootSymlink(t *testing.T) {
	useStore(t)
	target := newRoot(t)
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(link, "demo:link"); err == nil {
		t.Fatal("rootfs symlink unexpectedly accepted")
	}
}

func TestStoreLayoutIsValidOCIJSON(t *testing.T) {
	base := useStore(t)
	if _, err := ImportRootFS(newRoot(t), "demo:layout"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oci-layout", "index.json"} {
		data, err := os.ReadFile(filepath.Join(base, "images", name))
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("%s is invalid JSON: %v", name, err)
		}
	}
}

func TestRootFSManifestSanity(t *testing.T) {
	manifest, err := rootfs.Inspect(newRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Files == 0 {
		t.Fatal("rootfs fixture has no files")
	}
}

func TestSaveLoadAndPrune(t *testing.T) {
	base := useStore(t)
	if _, err := ImportRootFS(newRoot(t), "demo:portable"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "portable.oci.tar.gz")
	if err := Save("demo:portable", archive); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Remove("demo:portable"); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect("demo:portable"); err == nil {
		t.Fatal("removed image remained indexed")
	}
	// The tag was removed, so the blobs should now be eligible for collection.
	candidates, err := Prune(true)
	if err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("expected no-longer-reachable blobs to be prunable")
	}
	removed, err := Prune(false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("expected garbage collector to remove orphaned blobs")
	}
	loaded, err := Load(archive)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Reference != "demo:portable" {
		t.Fatalf("unexpected loaded images: %+v", loaded)
	}
	if _, err := Inspect("demo:portable"); err != nil {
		t.Fatalf("Inspect after load: %v", err)
	}
}

func TestUsageDetectsReclaimableBlobsAndVerifiesAllFiles(t *testing.T) {
	base := useStore(t)
	if _, err := ImportRootFS(newRoot(t), "demo:usage"); err != nil {
		t.Fatal(err)
	}
	usage, err := Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Images != 1 || usage.UniqueBlobs != 3 || usage.ReachableBlobs != 3 || usage.ReclaimableBlobs != 0 || usage.TotalBytes != usage.ReachableBytes {
		t.Fatalf("unexpected reachable usage: %+v", usage)
	}
	if err := Remove("demo:usage"); err != nil {
		t.Fatal(err)
	}
	usage, err = Usage()
	if err != nil {
		t.Fatalf("Usage after remove: %v", err)
	}
	if usage.Images != 0 || usage.ReclaimableBlobs != 3 || usage.ReclaimableBytes == 0 {
		t.Fatalf("unexpected reclaimable usage: %+v", usage)
	}
	blobDir := filepath.Join(base, "images", "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("read blob directory: %v", err)
	}
	path := filepath.Join(blobDir, entries[0].Name())
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Usage(); err == nil {
		t.Fatal("usage accepted a blob whose content does not match its filename")
	}
}

func TestLoadRejectsInvalidOCIArchive(t *testing.T) {
	base := useStore(t)
	archive := filepath.Join(base, "invalid.tar.gz")
	// Generate a valid gzip tar rootfs, but it is not an OCI image layout.
	root := newRoot(t)
	if _, err := rootfs.PackTarGz(root, archive); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(archive); err == nil {
		t.Fatal("non-OCI archive unexpectedly loaded")
	}
}

func TestInspectRejectsUncompressedDiffIDMismatch(t *testing.T) {
	base := useStore(t)
	if _, err := ImportRootFS(newRoot(t), "demo:diffid"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "images")
	index, err := loadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadManifest(dir, index.Manifests[0])
	if err != nil {
		t.Fatal(err)
	}
	configData, err := readBlob(dir, manifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	var config ImageConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		t.Fatal(err)
	}
	config.RootFS.DiffIDs[0] = "sha256:" + strings.Repeat("0", 64)
	configData, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configDigest := digestBytes(configData)
	if err := writeBlob(dir, configDigest, configData); err != nil {
		t.Fatal(err)
	}
	manifest.Config = Descriptor{MediaType: configType, Digest: configDigest, Size: int64(len(configData))}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := digestBytes(manifestData)
	if err := writeBlob(dir, manifestDigest, manifestData); err != nil {
		t.Fatal(err)
	}
	index.Manifests[0].Digest = manifestDigest
	index.Manifests[0].Size = int64(len(manifestData))
	if err := writeIndex(dir, index); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect("demo:diffid"); err == nil || !strings.Contains(err.Error(), "diff_id mismatch") {
		t.Fatalf("Inspect error = %v, want diff_id mismatch", err)
	}
}

func TestPruneRefusesUnexpectedBlobFilename(t *testing.T) {
	base := useStore(t)
	if _, err := ImportRootFS(newRoot(t), "demo:prune"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "images", "blobs", "sha256", "not-a-digest")
	if err := os.WriteFile(path, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(false); err == nil {
		t.Fatal("Prune accepted unexpected blob filename")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unexpected blob path was deleted: %v", err)
	}
}

func TestStoreRejectsSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real-store")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link-store")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOXR_IMAGE_DIR", link)
	if _, err := List(); err == nil {
		t.Fatal("symlinked image store unexpectedly accepted")
	}
}

func TestTagSharesManifestAndBlobs(t *testing.T) {
	useStore(t)
	created, err := ImportRootFS(newRoot(t), "demo:source")
	if err != nil {
		t.Fatal(err)
	}
	if err := Tag("demo:source", "demo:alias"); err != nil {
		t.Fatalf("Tag: %v", err)
	}
	original, err := Inspect("demo:source")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := Inspect("demo:alias")
	if err != nil {
		t.Fatal(err)
	}
	if original.Digest != alias.Digest || alias.Digest != created.Digest {
		t.Fatalf("tag did not share immutable manifest: source=%s alias=%s", original.Digest, alias.Digest)
	}
	if err := Remove("demo:source"); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect("demo:alias"); err != nil {
		t.Fatalf("alias lost when source tag removed: %v", err)
	}
	if candidates, err := Prune(true); err != nil || len(candidates) != 0 {
		t.Fatalf("Prune with live alias candidates=%v err=%v", candidates, err)
	}
}

func TestTagRejectsMissingSource(t *testing.T) {
	useStore(t)
	if err := Tag("missing:tag", "demo:alias"); err == nil {
		t.Fatal("Tag unexpectedly accepted missing source")
	}
}

func TestExportRegistryBundleCopiesVerifiedLayer(t *testing.T) {
	useStore(t)
	root := newRoot(t)
	created, err := ImportRootFS(root, "demo:registry-export")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ExportRegistryBundle(created.Reference)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Cleanup()
	if len(bundle.Manifest) == 0 || len(bundle.Config) == 0 || bundle.LayerPath == "" {
		t.Fatalf("incomplete registry bundle: %#v", bundle)
	}
	info, err := os.Lstat(bundle.LayerPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatalf("exported layer permissions/type: %v", info.Mode())
	}
	if _, err := os.Stat(bundle.LayerPath); err != nil {
		t.Fatal(err)
	}
}

func TestExportRegistryBundleRejectsMissingImage(t *testing.T) {
	useStore(t)
	if _, err := ExportRegistryBundle("missing:latest"); err == nil {
		t.Fatal("export of missing image unexpectedly succeeded")
	}
}
