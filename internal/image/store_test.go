package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

func TestPruneDetailedReportsBlobSizesAndHonorsDryRun(t *testing.T) {
	base := useStore(t)
	if _, err := ImportRootFS(newRoot(t), "demo:keep"); err != nil {
		t.Fatal(err)
	}
	payload := []byte("unreferenced payload for detailed prune")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	path := filepath.Join(base, "images", "blobs", "sha256", digest)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}

	items, err := PruneDetailed(true)
	if err != nil {
		t.Fatalf("PruneDetailed dry-run: %v", err)
	}
	if len(items) != 1 || items[0].Digest != "sha256:"+digest || items[0].SizeBytes != int64(len(payload)) {
		t.Fatalf("unexpected detailed prune plan: %#v", items)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dry-run removed unreferenced blob: %v", err)
	}

	items, err = PruneDetailed(false)
	if err != nil {
		t.Fatalf("PruneDetailed destructive run: %v", err)
	}
	if len(items) != 1 || items[0].Digest != "sha256:"+digest || items[0].SizeBytes != int64(len(payload)) {
		t.Fatalf("unexpected detailed prune result: %#v", items)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("destructive prune left unreferenced blob: %v", err)
	}
}

func TestSaveDockerArchiveRoundTrip(t *testing.T) {
	store := useStore(t)
	root := newRoot(t)
	runtimeConfig := ImageRuntimeConfig{
		Env:        []string{"APP_MODE=production"},
		Entrypoint: []string{"/bin/hello"},
		Cmd:        []string{"--serve"},
		WorkingDir: "/srv/app",
		User:       "1000:1000",
		Labels:     map[string]string{"org.example.component": "api"},
	}
	if _, err := ImportRootFSWithConfig(root, "example.com/demo:stable", "linux", "amd64", "", runtimeConfig); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(store, "docker-save.tar")
	if err := SaveDocker("example.com/demo:stable", archive); err != nil {
		t.Fatalf("SaveDocker: %v", err)
	}
	if !isDockerSaveArchive(archive) {
		t.Fatal("Docker export was not recognized as a Docker save archive")
	}

	stage := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := extractDockerArchive(archive, stage); err != nil {
		t.Fatalf("extract exported Docker archive: %v", err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []dockerManifestEntry
	if err := json.Unmarshal(manifestBytes, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Config == "" || len(entries[0].Layers) != 1 ||
		len(entries[0].RepoTags) != 1 || entries[0].RepoTags[0] != "example.com/demo:stable" {
		t.Fatalf("unexpected Docker save manifest: %+v", entries)
	}
	configBytes, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(entries[0].Config)))
	if err != nil {
		t.Fatal(err)
	}
	var savedConfig dockerSaveConfig
	if err := json.Unmarshal(configBytes, &savedConfig); err != nil {
		t.Fatal(err)
	}
	if savedConfig.OS != "linux" || savedConfig.Architecture != "amd64" ||
		len(savedConfig.RootFS.DiffIDs) != 1 ||
		savedConfig.Config.WorkingDir != "/srv/app" ||
		savedConfig.Config.User != "1000:1000" ||
		len(savedConfig.History) != 1 {
		t.Fatalf("Docker image config did not preserve metadata: %+v", savedConfig)
	}
	diffID := strings.TrimPrefix(savedConfig.RootFS.DiffIDs[0], "sha256:")
	repositoriesBytes, err := os.ReadFile(filepath.Join(stage, "repositories"))
	if err != nil {
		t.Fatal(err)
	}
	var repositories map[string]map[string]string
	if err := json.Unmarshal(repositoriesBytes, &repositories); err != nil {
		t.Fatal(err)
	}
	if repositories["example.com/demo"]["stable"] != diffID {
		t.Fatalf("legacy repository map does not reference the exported layer: %+v", repositories)
	}
	layerInfo, err := os.Stat(filepath.Join(stage, filepath.FromSlash(entries[0].Layers[0])))
	if err != nil || !layerInfo.Mode().IsRegular() {
		t.Fatalf("Docker layer archive missing: %v", err)
	}

	if err := SaveDocker("example.com/demo:stable", filepath.Join(os.Getenv("BOXR_IMAGE_DIR"), "unsafe.tar")); err == nil {
		t.Fatal("Docker export allowed a destination inside the image store")
	}
	storeAlias := filepath.Join(t.TempDir(), "store-link")
	if err := os.Symlink(os.Getenv("BOXR_IMAGE_DIR"), storeAlias); err != nil {
		t.Fatalf("create store alias: %v", err)
	}
	if err := SaveDocker("example.com/demo:stable", filepath.Join(storeAlias, "symlink-escape.tar")); err == nil {
		t.Fatal("Docker export followed a symlinked destination directory into the image store")
	}
	nestedDestination := filepath.Join(storeAlias, "new", "nested", "escape.tar")
	if err := SaveDockerMany([]string{"example.com/demo:stable"}, nestedDestination); err == nil {
		t.Fatal("Docker export accepted a nested symlink destination inside the image store")
	}
	if _, err := os.Lstat(filepath.Join(os.Getenv("BOXR_IMAGE_DIR"), "new")); !os.IsNotExist(err) {
		t.Fatalf("rejected Docker export created directories in the image store: %v", err)
	}
	loaded, err := Load(archive)
	if err != nil {
		t.Fatalf("Load exported Docker archive: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Reference != "example.com/demo:stable" {
		t.Fatalf("unexpected images after Docker archive round trip: %+v", loaded)
	}
	inspected, err := Inspect("example.com/demo:stable")
	if err != nil {
		t.Fatal(err)
	}
	if inspected.RuntimeConfig.WorkingDir != runtimeConfig.WorkingDir ||
		inspected.RuntimeConfig.User != runtimeConfig.User ||
		len(inspected.RuntimeConfig.Env) != 1 ||
		inspected.RuntimeConfig.Env[0] != runtimeConfig.Env[0] {
		t.Fatalf("runtime config changed during Docker save/load: %+v", inspected.RuntimeConfig)
	}
	unpacked := filepath.Join(t.TempDir(), "unpacked")
	if _, err := Unpack("example.com/demo:stable", unpacked); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(unpacked, "bin", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "hello image\n" {
		t.Fatalf("unexpected rootfs content after round trip: %q", contents)
	}
}

func TestSaveDockerManyDeduplicatesImagesAndLayers(t *testing.T) {
	useStore(t)
	root := newRoot(t)
	firstConfig := ImageRuntimeConfig{
		Env:        []string{"APP_MODE=production"},
		Entrypoint: []string{"/bin/hello"},
		WorkingDir: "/srv/app",
		Labels:     map[string]string{"org.example.component": "api"},
	}
	if _, err := ImportRootFSWithConfig(root, "example.com/demo:stable", "linux", "amd64", "", firstConfig); err != nil {
		t.Fatal(err)
	}
	if err := Tag("example.com/demo:stable", "example.com/demo:latest"); err != nil {
		t.Fatal(err)
	}
	secondConfig := ImageRuntimeConfig{Cmd: []string{"sidecar"}}
	if _, err := ImportRootFSWithConfig(root, "example.com/sidecar:v1", "linux", "amd64", "", secondConfig); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "bundle.tar")
	references := []string{
		"example.com/sidecar:v1",
		"example.com/demo:stable",
		"example.com/demo:latest",
	}
	if err := SaveDockerMany(references, archive); err != nil {
		t.Fatalf("SaveDockerMany: %v", err)
	}
	firstBytes, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	repeated := filepath.Join(t.TempDir(), "bundle-repeat.tar")
	if err := SaveDockerMany(references, repeated); err != nil {
		t.Fatalf("repeat SaveDockerMany: %v", err)
	}
	secondBytes, err := os.ReadFile(repeated)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("Docker archive export is not deterministic for the same image references")
	}

	stage := filepath.Join(t.TempDir(), "extracted")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := extractDockerArchive(archive, stage); err != nil {
		t.Fatalf("extract multi-image archive: %v", err)
	}
	manifestData, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []dockerSaveManifestEntry
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 2 {
		t.Fatalf("got %d manifest entries, want 2 (identical tags should be grouped): %+v", len(manifest), manifest)
	}
	var allTags []string
	layerNames := make(map[string]bool)
	for _, entry := range manifest {
		if len(entry.Layers) != 1 {
			t.Fatalf("unexpected layer list: %+v", entry)
		}
		layerNames[entry.Layers[0]] = true
		allTags = append(allTags, entry.RepoTags...)
	}
	sort.Strings(allTags)
	wantTags := []string{
		"example.com/demo:latest",
		"example.com/demo:stable",
		"example.com/sidecar:v1",
	}
	if !reflect.DeepEqual(allTags, wantTags) {
		t.Fatalf("manifest tags = %v, want %v", allTags, wantTags)
	}
	if len(layerNames) != 1 {
		t.Fatalf("shared layer should be stored once, found %d layer paths", len(layerNames))
	}
	layerID := strings.TrimSuffix(strings.TrimSuffix(manifest[0].Layers[0], "/layer.tar"), "/")
	if layerID == manifest[0].Layers[0] || len(layerID) != 64 {
		t.Fatalf("unexpected Docker layer path %q", manifest[0].Layers[0])
	}

	repositoriesData, err := os.ReadFile(filepath.Join(stage, "repositories"))
	if err != nil {
		t.Fatal(err)
	}
	var repositories map[string]map[string]string
	if err := json.Unmarshal(repositoriesData, &repositories); err != nil {
		t.Fatal(err)
	}
	for _, reference := range wantTags {
		repository, tag, err := splitDockerRepositoryTag(reference)
		if err != nil {
			t.Fatal(err)
		}
		if repositories[repository][tag] != layerID {
			t.Errorf("repositories map omitted %s or used the wrong layer: %+v", reference, repositories)
		}
	}

	if err := SaveDockerMany([]string{"example.com/demo:stable", "example.com/demo:stable"}, filepath.Join(t.TempDir(), "duplicate.tar")); err == nil {
		t.Fatal("duplicate Docker export references were accepted")
	}
	if err := SaveDockerMany(make([]string, maxDockerImageCount+1), filepath.Join(t.TempDir(), "too-many.tar")); err == nil {
		t.Fatal("Docker export accepted more than the supported image count")
	}

	loaded, err := Load(archive)
	if err != nil {
		t.Fatalf("load multi-image Docker archive: %v", err)
	}
	loadedReferences := make([]string, 0, len(loaded))
	for _, item := range loaded {
		loadedReferences = append(loadedReferences, item.Reference)
	}
	sort.Strings(loadedReferences)
	if !reflect.DeepEqual(loadedReferences, wantTags) {
		t.Fatalf("loaded references = %v, want %v", loadedReferences, wantTags)
	}
}

func TestRemoveManyIsAtomicWhenAnyReferenceIsMissing(t *testing.T) {
	useStore(t)
	root := newRoot(t)
	first, err := ImportRootFS(root, "demo:first")
	if err != nil {
		t.Fatal(err)
	}
	if err := Tag("demo:first", "demo:alias"); err != nil {
		t.Fatal(err)
	}
	secondRoot := newRoot(t)
	if err := os.WriteFile(filepath.Join(secondRoot, "bin", "hello"), []byte("second image\n"), 0755); err != nil {
		t.Fatal(err)
	}
	second, err := ImportRootFS(secondRoot, "demo:second")
	if err != nil {
		t.Fatal(err)
	}

	if err := RemoveMany([]string{"demo:first", "demo:missing"}); err == nil {
		t.Fatal("RemoveMany accepted a set containing a missing reference")
	}
	for _, reference := range []string{"demo:first", "demo:alias", "demo:second"} {
		if _, err := Verify(reference); err != nil {
			t.Fatalf("reference %s changed after rejected batch removal: %v", reference, err)
		}
	}

	if err := RemoveMany([]string{"demo:first", "demo:alias"}); err != nil {
		t.Fatalf("RemoveMany: %v", err)
	}
	if _, err := Inspect("demo:first"); err == nil {
		t.Fatal("primary reference remains after successful batch removal")
	}
	if _, err := Inspect("demo:alias"); err == nil {
		t.Fatal("alias reference remains after successful batch removal")
	}
	if remaining, err := Verify("demo:second"); err != nil || remaining.Digest != second.Digest {
		t.Fatalf("unrelated reference changed: image=%+v err=%v", remaining, err)
	}
	if first.Digest == second.Digest {
		t.Fatal("test fixtures unexpectedly share an image digest")
	}
}
