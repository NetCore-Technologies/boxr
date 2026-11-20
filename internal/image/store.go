// Package image stores Boxr root filesystems in a local OCI image-layout store.
// The filesystem layer uses a documented Boxr media type; this is an OCI
// layout, not a claim of compatibility with every Docker/OCI runtime.
package image

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/rootfs"
)

const (
	layoutVersion = "1.0.0"
	manifestType  = "application/vnd.oci.image.manifest.v1+json"
	configType    = "application/vnd.oci.image.config.v1+json"
	layerType     = "application/vnd.boxr.rootfs.v1.tar+gzip"
	maxJSONBytes  = 4 << 20
	maxBlobBytes  = int64(8 << 30)
)

var (
	storeMu sync.Mutex
	refRE   = regexp.MustCompile(`^(?:[A-Za-z0-9.-]+(?::[0-9]{1,5})?/)?[A-Za-z0-9][A-Za-z0-9._/-]{0,127}:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	hexRE   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
}

type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

type ImageConfig struct {
	Created      time.Time          `json:"created"`
	Architecture string             `json:"architecture"`
	OS           string             `json:"os"`
	Variant      string             `json:"variant,omitempty"`
	Config       ImageRuntimeConfig `json:"config,omitempty"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

type Image struct {
	Reference     string             `json:"reference"`
	Digest        string             `json:"digest"`
	Created       time.Time          `json:"created"`
	OS            string             `json:"os"`
	Arch          string             `json:"architecture"`
	Variant       string             `json:"variant,omitempty"`
	LayerBytes    int64              `json:"layerBytes"`
	Layer         Descriptor         `json:"layer"`
	DiffIDs       []string           `json:"-"`
	RuntimeConfig ImageRuntimeConfig `json:"-"`
}

// StoreUsage describes verified storage usage in the local image store.
// Reachable blobs are referenced by at least one image tag; reclaimable blobs
// are valid content-addressed files not referenced by the current index.
// PrunedBlob identifies an unreferenced content-addressed blob and its size.
type PrunedBlob struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

type StoreUsage struct {
	Images           int   `json:"images"`
	UniqueBlobs      int   `json:"uniqueBlobs"`
	ReachableBlobs   int   `json:"reachableBlobs"`
	ReclaimableBlobs int   `json:"reclaimableBlobs"`
	TotalBytes       int64 `json:"totalBytes"`
	ReachableBytes   int64 `json:"reachableBytes"`
	ReclaimableBytes int64 `json:"reclaimableBytes"`
}

func StoreDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("BOXR_IMAGE_DIR")); value != "" {
		return filepath.Abs(value)
	}
	if value := strings.TrimSpace(os.Getenv("BOXR_DATA_DIR")); value != "" {
		return filepath.Join(value, "images"), nil
	}
	if value := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); value != "" {
		return filepath.Join(value, "boxr", "images"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "boxr", "images"), nil
}

func ensureStore() (string, error) {
	dir, err := StoreDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create image store: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("image store must be a real directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	for _, child := range []string{"blobs", filepath.Join("blobs", "sha256")} {
		path := filepath.Join(dir, child)
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("unsafe image store path %s", path)
		}
	}
	layoutPath := filepath.Join(dir, "oci-layout")
	if _, err := os.Lstat(layoutPath); errors.Is(err, os.ErrNotExist) {
		if err := atomicWrite(layoutPath, []byte(`{"imageLayoutVersion":"1.0.0"}`+"\n"), 0600); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	var layout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := readJSON(layoutPath, &layout); err != nil {
		return "", err
	}
	if layout.ImageLayoutVersion != layoutVersion {
		return "", fmt.Errorf("unsupported OCI layout version %q", layout.ImageLayoutVersion)
	}
	indexPath := filepath.Join(dir, "index.json")
	if _, err := os.Lstat(indexPath); errors.Is(err, os.ErrNotExist) {
		if err := writeIndex(dir, Index{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []Descriptor{}}); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return dir, nil
}

func ImportRootFS(source, reference string) (Image, error) {
	return ImportRootFSForPlatform(source, reference, "linux", runtimeArchitecture(), "")
}

// ImportRootFSForPlatform stores a prepared rootfs with its declared target
// platform. This is important for cross-platform registry pulls: the image
// metadata describes the image, not the machine performing the import.
func ImportRootFSForPlatform(source, reference, osName, architecture, variant string) (Image, error) {
	return ImportRootFSWithConfig(source, reference, osName, architecture, variant, ImageRuntimeConfig{})
}

// ImportRootFSWithConfig imports a root filesystem and preserves its runtime
// defaults in the OCI image config. The config is metadata only; it is never
// executed during import.
func ImportRootFSWithConfig(source, reference, osName, architecture, variant string, runtimeConfig ImageRuntimeConfig) (Image, error) {
	var result Image
	if err := ValidateBuildSpec(BuildSpec{Version: 1, Base: "scratch", Config: runtimeConfig}); err != nil {
		return result, fmt.Errorf("invalid image runtime config: %w", err)
	}
	if osName != "linux" || architecture == "" {
		return result, fmt.Errorf("unsupported image platform %q/%q", osName, architecture)
	}
	if strings.ContainsAny(osName+architecture+variant, "/\\\x00\r\n") {
		return result, fmt.Errorf("invalid image platform")
	}
	if err := ValidateReference(reference); err != nil {
		return result, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return result, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return result, err
	}
	archive, err := os.CreateTemp("", "boxr-image-layer-*.tar.gz")
	if err != nil {
		return result, err
	}
	archivePath := archive.Name()
	_ = archive.Close()
	defer os.Remove(archivePath)
	sourceInfo, sourceErr := os.Lstat(source)
	if sourceErr != nil {
		return result, fmt.Errorf("inspect image rootfs: %w", sourceErr)
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.IsDir() {
		return result, fmt.Errorf("image rootfs must be a real directory, not a symlink")
	}
	manifestRoot, err := rootfs.PackTarGz(source, archivePath)
	if err != nil {
		return result, fmt.Errorf("pack image rootfs: %w", err)
	}
	layerDigest, layerSize, err := digestFile(archivePath, maxBlobBytes)
	if err != nil {
		return result, err
	}
	if err := writeBlobFromFile(dir, layerDigest, archivePath, layerSize); err != nil {
		return result, err
	}
	diffID, err := uncompressedDigestFile(archivePath)
	if err != nil {
		return result, err
	}
	config := ImageConfig{Created: time.Now().UTC(), Architecture: architecture, OS: osName, Variant: variant, Config: runtimeConfig}
	config.RootFS.Type = "layers"
	config.RootFS.DiffIDs = []string{diffID}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return result, err
	}
	configDigest := digestBytes(configBytes)
	if err := writeBlob(dir, configDigest, configBytes); err != nil {
		return result, err
	}
	manifest := Manifest{SchemaVersion: 2, MediaType: manifestType,
		Config: Descriptor{MediaType: configType, Digest: configDigest, Size: int64(len(configBytes))},
		Layers: []Descriptor{{MediaType: layerType, Digest: layerDigest, Size: layerSize}}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return result, err
	}
	manifestDigest := digestBytes(manifestBytes)
	if err := writeBlob(dir, manifestDigest, manifestBytes); err != nil {
		return result, err
	}
	platform := &Platform{OS: config.OS, Architecture: config.Architecture, Variant: config.Variant}
	descriptor := Descriptor{MediaType: manifestType, Digest: manifestDigest, Size: int64(len(manifestBytes)), Platform: platform,
		Annotations: map[string]string{"org.opencontainers.image.ref.name": reference, "org.opencontainers.image.created": config.Created.Format(time.RFC3339Nano), "io.boxr.rootfs.files": fmt.Sprint(manifestRoot.Files)}}
	updated := make([]Descriptor, 0, len(index.Manifests)+1)
	for _, existing := range index.Manifests {
		if existing.Annotations["org.opencontainers.image.ref.name"] != reference {
			updated = append(updated, existing)
		}
	}
	updated = append(updated, descriptor)
	index.Manifests = updated
	if err := writeIndex(dir, index); err != nil {
		return result, err
	}
	return Image{
		Reference: reference, Digest: manifestDigest, Created: config.Created,
		OS: config.OS, Arch: config.Architecture, Variant: config.Variant,
		LayerBytes: layerSize, Layer: manifest.Layers[0],
		DiffIDs: []string{diffID}, RuntimeConfig: runtimeConfig,
	}, nil
}

func List() ([]Image, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return nil, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return nil, err
	}
	images := make([]Image, 0, len(index.Manifests))
	for _, descriptor := range index.Manifests {
		ref := descriptor.Annotations["org.opencontainers.image.ref.name"]
		item, err := inspectDescriptor(dir, ref, descriptor)
		if err != nil {
			return nil, err
		}
		images = append(images, item)
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Reference < images[j].Reference })
	return images, nil
}

func Inspect(reference string) (Image, error) {
	if err := ValidateReference(reference); err != nil {
		return Image{}, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return Image{}, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return Image{}, err
	}
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations["org.opencontainers.image.ref.name"] == reference {
			return inspectDescriptor(dir, reference, descriptor)
		}
	}
	return Image{}, fmt.Errorf("image %q not found", reference)
}

// Verify performs a complete descriptor and payload integrity check for a tag.
// It validates the manifest, config, compressed layer and uncompressed diff ID.
func Verify(reference string) (Image, error) {
	return Inspect(reference)
}

// Usage verifies every blob in the local store and reports the unique bytes
// reachable from tags separately from reclaimable, unreferenced blobs.
func Usage() (StoreUsage, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	var usage StoreUsage
	dir, err := ensureStore()
	if err != nil {
		return usage, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return usage, err
	}
	usage.Images = len(index.Manifests)
	reachable := make(map[string]bool)
	for _, descriptor := range index.Manifests {
		reference := descriptor.Annotations["org.opencontainers.image.ref.name"]
		if _, err := inspectDescriptor(dir, reference, descriptor); err != nil {
			return usage, fmt.Errorf("verify image %s: %w", reference, err)
		}
		manifest, err := loadManifest(dir, descriptor)
		if err != nil {
			return usage, err
		}
		reachable[strings.TrimPrefix(descriptor.Digest, "sha256:")] = true
		reachable[strings.TrimPrefix(manifest.Config.Digest, "sha256:")] = true
		for _, layer := range manifest.Layers {
			reachable[strings.TrimPrefix(layer.Digest, "sha256:")] = true
		}
	}
	blobDir := filepath.Join(dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		return usage, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !hexRE.MatchString(entry.Name()) {
			return usage, fmt.Errorf("unexpected entry in OCI blob directory: %s", entry.Name())
		}
		path := filepath.Join(blobDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return usage, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxBlobBytes {
			return usage, fmt.Errorf("refusing to inspect unsafe or oversized blob %s", entry.Name())
		}
		digest, size, err := digestFile(path, maxBlobBytes)
		if err != nil {
			return usage, err
		}
		if digest != "sha256:"+entry.Name() {
			return usage, fmt.Errorf("blob filename digest mismatch for %s", entry.Name())
		}
		usage.UniqueBlobs++
		usage.TotalBytes += size
		if reachable[entry.Name()] {
			usage.ReachableBlobs++
			usage.ReachableBytes += size
		} else {
			usage.ReclaimableBlobs++
			usage.ReclaimableBytes += size
		}
	}
	return usage, nil
}

func Remove(reference string) error {
	if err := ValidateReference(reference); err != nil {
		return err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return err
	}
	updated := make([]Descriptor, 0, len(index.Manifests))
	found := false
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations["org.opencontainers.image.ref.name"] == reference {
			found = true
			continue
		}
		updated = append(updated, descriptor)
	}
	if !found {
		return fmt.Errorf("image %q not found", reference)
	}
	index.Manifests = updated
	return writeIndex(dir, index)
}

func Unpack(reference, destination string) (rootfs.Manifest, error) {
	if err := ValidateReference(reference); err != nil {
		return rootfs.Manifest{}, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return rootfs.Manifest{}, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return rootfs.Manifest{}, err
	}
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations["org.opencontainers.image.ref.name"] != reference {
			continue
		}
		if _, err := inspectDescriptor(dir, reference, descriptor); err != nil {
			return rootfs.Manifest{}, fmt.Errorf("verify image before unpack: %w", err)
		}
		manifest, err := loadManifest(dir, descriptor)
		if err != nil {
			return rootfs.Manifest{}, err
		}
		if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != layerType {
			return rootfs.Manifest{}, fmt.Errorf("image %q contains an unsupported layer layout", reference)
		}
		layerPath, err := verifyBlob(dir, manifest.Layers[0])
		if err != nil {
			return rootfs.Manifest{}, err
		}
		return rootfs.ExtractTarGz(layerPath, destination, rootfs.Limits{})
	}
	return rootfs.Manifest{}, fmt.Errorf("image %q not found", reference)
}

func ValidateReference(reference string) error {
	if !refRE.MatchString(reference) || strings.Contains(reference, "..") || strings.Contains(reference, "//") || strings.HasSuffix(reference, "/") {
		return fmt.Errorf("invalid image reference %q (expected name:tag)", reference)
	}
	return nil
}

func inspectDescriptor(dir, reference string, descriptor Descriptor) (Image, error) {
	manifest, err := loadManifest(dir, descriptor)
	if err != nil {
		return Image{}, err
	}
	if manifest.Config.MediaType != configType {
		return Image{}, fmt.Errorf("unsupported image config media type %q", manifest.Config.MediaType)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != layerType {
		return Image{}, fmt.Errorf("image must contain exactly one Boxr rootfs layer")
	}
	layer := manifest.Layers[0]
	layerPath, err := verifyBlob(dir, layer)
	if err != nil {
		return Image{}, fmt.Errorf("verify image layer: %w", err)
	}
	configData, err := readBlob(dir, manifest.Config)
	if err != nil {
		return Image{}, err
	}
	var config ImageConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return Image{}, fmt.Errorf("decode image config: %w", err)
	}
	if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != 1 {
		return Image{}, fmt.Errorf("image config has an invalid rootfs descriptor")
	}
	diffID, err := uncompressedDigestFile(layerPath)
	if err != nil {
		return Image{}, fmt.Errorf("verify uncompressed image layer: %w", err)
	}
	if diffID != config.RootFS.DiffIDs[0] {
		return Image{}, fmt.Errorf("image layer diff_id mismatch")
	}
	if config.OS == "" || config.Architecture == "" {
		return Image{}, fmt.Errorf("image config is missing platform metadata")
	}
	return Image{
		Reference: reference, Digest: descriptor.Digest, Created: config.Created,
		OS: config.OS, Arch: config.Architecture, Variant: config.Variant,
		LayerBytes: layer.Size, Layer: layer,
		DiffIDs:       append([]string(nil), config.RootFS.DiffIDs...),
		RuntimeConfig: config.Config,
	}, nil
}

func loadManifest(dir string, descriptor Descriptor) (Manifest, error) {
	var manifest Manifest
	if descriptor.MediaType != manifestType {
		return manifest, fmt.Errorf("unsupported manifest media type %q", descriptor.MediaType)
	}
	data, err := readBlob(dir, descriptor)
	if err != nil {
		return manifest, err
	}
	if int64(len(data)) != descriptor.Size {
		return manifest, fmt.Errorf("manifest size mismatch")
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != manifestType {
		return manifest, fmt.Errorf("invalid image manifest")
	}
	return manifest, nil
}

func loadIndex(dir string) (Index, error) {
	var index Index
	if err := readJSON(filepath.Join(dir, "index.json"), &index); err != nil {
		return index, err
	}
	if index.SchemaVersion != 2 {
		return index, fmt.Errorf("invalid OCI index schema version %d", index.SchemaVersion)
	}
	seen := make(map[string]bool)
	for _, item := range index.Manifests {
		ref := item.Annotations["org.opencontainers.image.ref.name"]
		if err := ValidateReference(ref); err != nil {
			return index, fmt.Errorf("invalid image index reference: %w", err)
		}
		if seen[ref] {
			return index, fmt.Errorf("duplicate image reference %q", ref)
		}
		seen[ref] = true
	}
	return index, nil
}

func readBlob(dir string, descriptor Descriptor) ([]byte, error) {
	if descriptor.Size > maxJSONBytes {
		return nil, fmt.Errorf("JSON OCI blob %s exceeds %d bytes", descriptor.Digest, maxJSONBytes)
	}
	path, err := verifyBlob(dir, descriptor)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// verifyBlob streams a blob through SHA-256 so inspecting large rootfs layers
// does not allocate memory proportional to the image size.
func verifyBlob(dir string, descriptor Descriptor) (string, error) {
	algorithm, value, ok := strings.Cut(descriptor.Digest, ":")
	if !ok || algorithm != "sha256" || !hexRE.MatchString(value) || descriptor.Size < 0 || descriptor.Size > maxBlobBytes {
		return "", fmt.Errorf("invalid OCI descriptor")
	}
	path := filepath.Join(dir, "blobs", "sha256", value)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != descriptor.Size {
		return "", fmt.Errorf("OCI blob metadata mismatch for %s", descriptor.Digest)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != descriptor.Digest {
		return "", fmt.Errorf("OCI blob digest mismatch for %s", descriptor.Digest)
	}
	return path, nil
}

func writeBlob(dir, digest string, data []byte) error {
	if !strings.HasPrefix(digest, "sha256:") || !hexRE.MatchString(strings.TrimPrefix(digest, "sha256:")) {
		return fmt.Errorf("invalid blob digest %q", digest)
	}
	path := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if existing, err := os.Lstat(path); err == nil {
		if _, verifyErr := verifyBlob(dir, Descriptor{Digest: digest, Size: int64(len(data))}); verifyErr != nil {
			return fmt.Errorf("existing blob %s failed verification: %w", digest, verifyErr)
		}
		_ = existing
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if digestBytes(data) != digest {
		return fmt.Errorf("data does not match blob digest %s", digest)
	}
	return atomicWrite(path, data, 0600)
}

func writeBlobFromFile(dir, digest, source string, size int64) error {
	if !strings.HasPrefix(digest, "sha256:") || !hexRE.MatchString(strings.TrimPrefix(digest, "sha256:")) || size < 0 || size > maxBlobBytes {
		return fmt.Errorf("invalid blob descriptor")
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() != size {
		return fmt.Errorf("blob source must be a regular file with the declared size")
	}
	path := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if _, err := os.Lstat(path); err == nil {
		if _, verifyErr := verifyBlob(dir, Descriptor{Digest: digest, Size: size}); verifyErr != nil {
			return fmt.Errorf("existing blob %s failed verification: %w", digest, verifyErr)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	actualDigest, actualSize, err := digestFile(source, maxBlobBytes)
	if err != nil {
		return err
	}
	if actualDigest != digest || actualSize != size {
		return fmt.Errorf("blob source digest or size mismatch")
	}
	return copyFileAtomic(source, path, 0600)
}

func copyFileAtomic(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(destination), ".boxr-image-*.tmp")
	if err != nil {
		return err
	}
	temporary := output.Name()
	committed := false
	defer func() {
		_ = output.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	if err := output.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	committed = true
	return nil
}

func digestFile(path string, maxBytes int64) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		return "", 0, fmt.Errorf("blob exceeds the %d-byte limit or is not a regular file", maxBytes)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maxBytes+1))
	if err != nil {
		return "", written, err
	}
	if written > maxBytes {
		return "", written, fmt.Errorf("blob exceeds the %d-byte limit", maxBytes)
	}
	if written != info.Size() {
		return "", written, fmt.Errorf("file changed while hashing")
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), written, nil
}

func uncompressedDigestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(reader, maxBlobBytes+1))
	if err != nil {
		return "", err
	}
	if written > maxBlobBytes {
		return "", fmt.Errorf("uncompressed image layer exceeds %d bytes", maxBlobBytes)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func writeIndex(dir string, index Index) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "index.json"), append(data, '\n'), 0600)
}

func readJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxJSONBytes {
		return fmt.Errorf("unsafe or oversized JSON file %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxJSONBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("JSON file %s contains trailing data", path)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".boxr-image-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func runtimeArchitecture() string {
	// Images are initially tagged for the architecture of the build host.
	// A future manifest-list workflow can add additional platform descriptors.
	return runtime.GOARCH
}

// Save writes a portable OCI image-layout archive containing one selected tag
// and only the blobs reachable from its manifest.
func Save(reference, archivePath string) error {
	if err := ValidateReference(reference); err != nil {
		return err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return err
	}
	var selected *Descriptor
	for i := range index.Manifests {
		if index.Manifests[i].Annotations["org.opencontainers.image.ref.name"] == reference {
			copy := index.Manifests[i]
			selected = &copy
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("image %q not found", reference)
	}
	if _, err := inspectDescriptor(dir, reference, *selected); err != nil {
		return fmt.Errorf("verify image before save: %w", err)
	}
	manifest, err := loadManifest(dir, *selected)
	if err != nil {
		return err
	}
	blobs := []Descriptor{*selected, manifest.Config}
	blobs = append(blobs, manifest.Layers...)
	stage, err := os.MkdirTemp("", "boxr-oci-save-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.MkdirAll(filepath.Join(stage, "blobs", "sha256"), 0700); err != nil {
		return err
	}
	for _, descriptor := range blobs {
		sourcePath, err := verifyBlob(dir, descriptor)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(descriptor.Digest, "sha256:")
		if err := copyFileAtomic(sourcePath, filepath.Join(stage, "blobs", "sha256", name), 0600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`+"\n"), 0600); err != nil {
		return err
	}
	if err := writeIndex(stage, Index{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []Descriptor{*selected}}); err != nil {
		return err
	}
	_, err = rootfs.PackTarGz(stage, archivePath)
	return err
}

// Load imports a portable OCI image-layout archive. Every descriptor and blob
// is verified before the local index is atomically updated.
func Load(archivePath string) ([]Image, error) {
	if isDockerSaveArchive(archivePath) {
		return LoadDockerArchive(archivePath)
	}

	stageParent, err := os.MkdirTemp("", "boxr-oci-load-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageParent)
	stage := filepath.Join(stageParent, "layout")
	if _, err := rootfs.ExtractTarGz(archivePath, stage, rootfs.Limits{}); err != nil {
		return nil, fmt.Errorf("extract OCI archive: %w", err)
	}
	var layout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := readJSON(filepath.Join(stage, "oci-layout"), &layout); err != nil {
		return nil, err
	}
	if layout.ImageLayoutVersion != layoutVersion {
		return nil, fmt.Errorf("unsupported OCI layout version %q", layout.ImageLayoutVersion)
	}
	incoming, err := loadIndex(stage)
	if err != nil {
		return nil, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return nil, err
	}
	current, err := loadIndex(dir)
	if err != nil {
		return nil, err
	}
	results := make([]Image, 0, len(incoming.Manifests))
	for _, descriptor := range incoming.Manifests {
		manifest, err := loadManifest(stage, descriptor)
		if err != nil {
			return nil, err
		}
		configData, err := readBlob(stage, manifest.Config)
		if err != nil {
			return nil, err
		}
		layerPaths := make([]string, 0, len(manifest.Layers))
		for _, layer := range manifest.Layers {
			layerPath, err := verifyBlob(stage, layer)
			if err != nil {
				return nil, err
			}
			layerPaths = append(layerPaths, layerPath)
		}
		if err := writeBlob(dir, manifest.Config.Digest, configData); err != nil {
			return nil, err
		}
		for index, layer := range manifest.Layers {
			if err := writeBlobFromFile(dir, layer.Digest, layerPaths[index], layer.Size); err != nil {
				return nil, err
			}
		}
		manifestData, err := readBlob(stage, descriptor)
		if err != nil {
			return nil, err
		}
		if err := writeBlob(dir, descriptor.Digest, manifestData); err != nil {
			return nil, err
		}
		ref := descriptor.Annotations["org.opencontainers.image.ref.name"]
		filtered := make([]Descriptor, 0, len(current.Manifests)+1)
		for _, old := range current.Manifests {
			if old.Annotations["org.opencontainers.image.ref.name"] != ref {
				filtered = append(filtered, old)
			}
		}
		filtered = append(filtered, descriptor)
		current.Manifests = filtered
		item, err := inspectDescriptor(dir, ref, descriptor)
		if err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	if err := writeIndex(dir, current); err != nil {
		return nil, err
	}
	return results, nil
}

// Prune removes content-addressed blobs not reachable from any local tag.
// When dryRun is true it reports candidates without deleting them.
func Prune(dryRun bool) ([]string, error) {
	blobs, err := PruneDetailed(dryRun)
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(blobs))
	for _, blob := range blobs {
		digests = append(digests, blob.Digest)
	}
	return digests, nil
}

func PruneDetailed(dryRun bool) ([]PrunedBlob, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return nil, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return nil, err
	}
	reachable := make(map[string]bool)
	for _, descriptor := range index.Manifests {
		ref := descriptor.Annotations["org.opencontainers.image.ref.name"]
		if _, err := inspectDescriptor(dir, ref, descriptor); err != nil {
			return nil, fmt.Errorf("verify image %s before pruning: %w", ref, err)
		}
		manifest, err := loadManifest(dir, descriptor)
		if err != nil {
			return nil, err
		}
		reachable[strings.TrimPrefix(descriptor.Digest, "sha256:")] = true
		reachable[strings.TrimPrefix(manifest.Config.Digest, "sha256:")] = true
		for _, layer := range manifest.Layers {
			reachable[strings.TrimPrefix(layer.Digest, "sha256:")] = true
		}
	}
	blobDir := filepath.Join(dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		return nil, err
	}
	removed := make([]PrunedBlob, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !hexRE.MatchString(entry.Name()) {
			return nil, fmt.Errorf("unexpected file in OCI blob directory: %s", entry.Name())
		}
		if reachable[entry.Name()] {
			continue
		}
		path := filepath.Join(blobDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing to prune non-regular blob path %s", path)
		}
		removed = append(removed, PrunedBlob{Digest: "sha256:" + entry.Name(), SizeBytes: info.Size()})
		if !dryRun {
			if err := os.Remove(path); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].Digest < removed[j].Digest })
	return removed, nil
}

// Tag creates or replaces a local reference to the same immutable manifest.
// Existing blob data is shared; no image layers are copied.
func Tag(source, target string) error {
	if err := ValidateReference(source); err != nil {
		return err
	}
	if err := ValidateReference(target); err != nil {
		return err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return err
	}
	var selected *Descriptor
	for i := range index.Manifests {
		if index.Manifests[i].Annotations["org.opencontainers.image.ref.name"] == source {
			copy := index.Manifests[i]
			selected = &copy
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("source image %q not found", source)
	}
	if _, err := inspectDescriptor(dir, source, *selected); err != nil {
		return fmt.Errorf("verify source image: %w", err)
	}
	annotations := make(map[string]string, len(selected.Annotations)+1)
	for key, value := range selected.Annotations {
		annotations[key] = value
	}
	annotations["org.opencontainers.image.ref.name"] = target
	selected.Annotations = annotations
	updated := make([]Descriptor, 0, len(index.Manifests)+1)
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations["org.opencontainers.image.ref.name"] != target {
			updated = append(updated, descriptor)
		}
	}
	updated = append(updated, *selected)
	index.Manifests = updated
	return writeIndex(dir, index)
}

// RegistryBundle is a verified single-rootfs image ready for OCI Distribution
// upload. The layer is copied to a private temporary file so a concurrent local
// prune cannot invalidate the upload after this method releases the store lock.
type RegistryBundle struct {
	Manifest  []byte
	Config    []byte
	LayerPath string
}

func (bundle RegistryBundle) Cleanup() error {
	if bundle.LayerPath == "" {
		return nil
	}
	return os.Remove(bundle.LayerPath)
}

// ExportRegistryBundle validates every referenced local blob before returning
// the manifest/config bytes and a stable temporary copy of the rootfs layer.
func ExportRegistryBundle(reference string) (RegistryBundle, error) {
	var bundle RegistryBundle
	if err := ValidateReference(reference); err != nil {
		return bundle, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureStore()
	if err != nil {
		return bundle, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		return bundle, err
	}
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations["org.opencontainers.image.ref.name"] != reference {
			continue
		}
		if _, err := inspectDescriptor(dir, reference, descriptor); err != nil {
			return bundle, fmt.Errorf("verify image before registry export: %w", err)
		}
		manifest, err := loadManifest(dir, descriptor)
		if err != nil {
			return bundle, err
		}
		manifestBytes, err := readBlob(dir, descriptor)
		if err != nil {
			return bundle, err
		}
		configBytes, err := readBlob(dir, manifest.Config)
		if err != nil {
			return bundle, err
		}
		layerPath, err := verifyBlob(dir, manifest.Layers[0])
		if err != nil {
			return bundle, err
		}
		source, err := os.Open(layerPath)
		if err != nil {
			return bundle, err
		}
		temporary, err := os.CreateTemp("", "boxr-registry-export-*.tar.gz")
		if err != nil {
			_ = source.Close()
			return bundle, err
		}
		temporaryPath := temporary.Name()
		cleanup := true
		defer func() {
			if cleanup {
				_ = os.Remove(temporaryPath)
			}
		}()
		if err := temporary.Chmod(0600); err != nil {
			_ = source.Close()
			_ = temporary.Close()
			return bundle, err
		}
		_, copyErr := io.Copy(temporary, source)
		sourceCloseErr := source.Close()
		syncErr := temporary.Sync()
		closeErr := temporary.Close()
		for _, copyError := range []error{copyErr, sourceCloseErr, syncErr, closeErr} {
			if copyError != nil {
				return bundle, copyError
			}
		}
		cleanup = false
		bundle.Manifest = manifestBytes
		bundle.Config = configBytes
		bundle.LayerPath = temporaryPath
		return bundle, nil
	}
	return bundle, fmt.Errorf("image %q not found", reference)
}
