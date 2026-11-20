package image

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type dockerSaveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type dockerSaveHistory struct {
	Created   time.Time `json:"created,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
}

type dockerSaveConfig struct {
	ImageConfig
	History []dockerSaveHistory `json:"history,omitempty"`
}

// SaveDocker writes a standard Docker-save-compatible tar archive for one
// local image tag. The OCI save format remains available through Save.
type dockerSaveLayer struct {
	path string
	size int64
}

// SaveDocker writes a standard Docker-save-compatible tar archive for one
// local image tag. The OCI save format remains available through Save.
func SaveDocker(reference, archivePath string) error {
	return SaveDockerMany([]string{reference}, archivePath)
}

// SaveDockerMany writes up to 128 local tags into one Docker-save archive.
// Repeated image configurations are combined into a single manifest entry,
// and shared rootfs layers are written only once.
func SaveDockerMany(references []string, archivePath string) error {
	if len(references) == 0 || len(references) > maxDockerImageCount {
		return fmt.Errorf("Docker export requires between 1 and %d image references", maxDockerImageCount)
	}
	if archivePath == "" {
		return fmt.Errorf("Docker archive path must not be empty")
	}
	refs := append([]string(nil), references...)
	sort.Strings(refs)
	for index, reference := range refs {
		if err := ValidateReference(reference); err != nil {
			return err
		}
		if index > 0 && refs[index-1] == reference {
			return fmt.Errorf("duplicate Docker export reference %q", reference)
		}
	}

	storeMu.Lock()
	defer storeMu.Unlock()

	dir, err := ensureStore()
	if err != nil {
		return err
	}
	storeAbs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	storeResolved, err := filepath.EvalSymlinks(storeAbs)
	if err != nil {
		return fmt.Errorf("resolve image store directory: %w", err)
	}
	destination, err := filepath.Abs(archivePath)
	if err != nil {
		return err
	}
	if err := validateDockerArchiveDestination(storeResolved, destination); err != nil {
		return err
	}

	index, err := loadIndex(dir)
	if err != nil {
		return err
	}
	descriptors := make(map[string]Descriptor, len(index.Manifests))
	for _, descriptor := range index.Manifests {
		reference := descriptor.Annotations["org.opencontainers.image.ref.name"]
		descriptors[reference] = descriptor
	}

	stage, err := os.MkdirTemp("", "boxr-docker-save-*")
	if err != nil {
		return fmt.Errorf("create Docker save workspace: %w", err)
	}
	defer os.RemoveAll(stage)

	layers := make(map[string]dockerSaveLayer)
	var totalLayerBytes int64
	images := make(map[string]*dockerSaveManifestEntry)
	configs := make(map[string][]byte)
	repositories := make(map[string]map[string]string)
	for _, reference := range refs {
		descriptor, ok := descriptors[reference]
		if !ok {
			return fmt.Errorf("image %q not found", reference)
		}
		if _, err := inspectDescriptor(dir, reference, descriptor); err != nil {
			return fmt.Errorf("verify image %q before Docker save: %w", reference, err)
		}
		manifest, err := loadManifest(dir, descriptor)
		if err != nil {
			return fmt.Errorf("load image %q manifest: %w", reference, err)
		}
		if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != layerType {
			return fmt.Errorf("image %q has an unsupported layer layout", reference)
		}
		configData, err := readBlob(dir, manifest.Config)
		if err != nil {
			return fmt.Errorf("read image %q config: %w", reference, err)
		}
		var config ImageConfig
		if err := json.Unmarshal(configData, &config); err != nil {
			return fmt.Errorf("decode image %q config: %w", reference, err)
		}
		if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != 1 ||
			!validDockerDiffID(config.RootFS.DiffIDs[0]) {
			return fmt.Errorf("image %q has an invalid rootfs diff_id", reference)
		}
		layerID := strings.TrimPrefix(config.RootFS.DiffIDs[0], "sha256:")
		layerName := layerID + "/layer.tar"
		if _, exists := layers[layerID]; !exists {
			layerPath, err := verifyBlob(dir, manifest.Layers[0])
			if err != nil {
				return fmt.Errorf("verify image %q layer: %w", reference, err)
			}
			uncompressedLayer := filepath.Join(stage, layerID+".layer.tar")
			layerFile, err := os.OpenFile(uncompressedLayer, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			if err := decompressDockerLayer(layerPath, layerFile, config.RootFS.DiffIDs[0]); err != nil {
				_ = layerFile.Close()
				return fmt.Errorf("prepare image %q Docker layer: %w", reference, err)
			}
			layerInfo, err := layerFile.Stat()
			if err != nil {
				_ = layerFile.Close()
				return err
			}
			if layerInfo.Size() > maxDockerArchiveBytes-totalLayerBytes {
				_ = layerFile.Close()
				return fmt.Errorf("Docker export exceeds the %d-byte total layer limit", maxDockerArchiveBytes)
			}
			totalLayerBytes += layerInfo.Size()
			if err := layerFile.Close(); err != nil {
				return err
			}
			layers[layerID] = dockerSaveLayer{path: uncompressedLayer, size: layerInfo.Size()}
		}

		dockerConfig := dockerSaveConfig{
			ImageConfig: config,
			History: []dockerSaveHistory{{
				Created:   config.Created,
				CreatedBy: "boxr image save --format docker",
			}},
		}
		exportConfig, err := json.Marshal(dockerConfig)
		if err != nil {
			return fmt.Errorf("encode Docker config for %q: %w", reference, err)
		}
		configDigest := digestBytes(exportConfig)
		configName := strings.TrimPrefix(configDigest, "sha256:") + ".json"
		configs[configName] = exportConfig
		groupKey := configDigest + ":" + layerID
		entry := images[groupKey]
		if entry == nil {
			entry = &dockerSaveManifestEntry{
				Config:   configName,
				RepoTags: []string{},
				Layers:   []string{layerName},
			}
			images[groupKey] = entry
		}
		entry.RepoTags = append(entry.RepoTags, reference)

		repository, tag, err := splitDockerRepositoryTag(reference)
		if err != nil {
			return err
		}
		if repositories[repository] == nil {
			repositories[repository] = make(map[string]string)
		}
		if previous, exists := repositories[repository][tag]; exists && previous != layerID {
			return fmt.Errorf("Docker repository tag %s:%s refers to conflicting layers", repository, tag)
		}
		repositories[repository][tag] = layerID
	}

	groupKeys := make([]string, 0, len(images))
	for key := range images {
		groupKeys = append(groupKeys, key)
	}
	sort.Strings(groupKeys)
	manifestEntries := make([]dockerSaveManifestEntry, 0, len(groupKeys))
	for _, key := range groupKeys {
		entry := *images[key]
		sort.Strings(entry.RepoTags)
		manifestEntries = append(manifestEntries, entry)
	}
	manifestBytes, err := json.Marshal(manifestEntries)
	if err != nil {
		return err
	}
	repositoriesBytes, err := json.Marshal(repositories)
	if err != nil {
		return err
	}
	configNames := make([]string, 0, len(configs))
	for name := range configs {
		configNames = append(configNames, name)
	}
	sort.Strings(configNames)
	layerIDs := make([]string, 0, len(layers))
	for id := range layers {
		layerIDs = append(layerIDs, id)
	}
	sort.Strings(layerIDs)

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve Docker archive destination directory: %w", err)
	}
	if err := validateDockerArchiveDestination(storeResolved, filepath.Join(resolvedParent, filepath.Base(destination))); err != nil {
		return fmt.Errorf("Docker archive destination resolves inside image store: %w", err)
	}
	output, err := os.CreateTemp(parent, ".boxr-docker-save-*.tmp")
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

	writer := tar.NewWriter(output)
	archiveTime := time.Unix(0, 0).UTC()
	write := func(name string, data []byte) error {
		return writeDockerTarBytes(writer, name, data, archiveTime)
	}
	if err := write("manifest.json", manifestBytes); err != nil {
		_ = writer.Close()
		return err
	}
	for _, name := range configNames {
		if err := write(name, configs[name]); err != nil {
			_ = writer.Close()
			return err
		}
	}
	if err := write("repositories", repositoriesBytes); err != nil {
		_ = writer.Close()
		return err
	}
	for _, layerID := range layerIDs {
		layer := layers[layerID]
		if err := writeDockerTarDirectory(writer, layerID+"/", archiveTime); err != nil {
			_ = writer.Close()
			return err
		}
		if err := writeDockerTarFile(writer, layerID+"/layer.tar", layer.path, layer.size, archiveTime); err != nil {
			_ = writer.Close()
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close Docker archive: %w", err)
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("publish Docker archive: %w", err)
	}
	committed = true
	return nil
}

func validateDockerArchiveDestination(storeDir, destination string) error {
	relative, err := filepath.Rel(storeDir, destination)
	if err != nil {
		return err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return fmt.Errorf("Docker archive destination must be outside the image store")
	}
	return nil
}

func splitDockerRepositoryTag(reference string) (string, string, error) {
	separator := strings.LastIndex(reference, ":")
	if separator <= 0 || separator == len(reference)-1 {
		return "", "", fmt.Errorf("invalid image reference %q", reference)
	}
	repository, tag := reference[:separator], reference[separator+1:]
	if strings.Contains(tag, "/") {
		return "", "", fmt.Errorf("invalid image tag in %q", reference)
	}
	return repository, tag, nil
}

func decompressDockerLayer(source string, destination *os.File, expectedDiffID string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	reader, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("open compressed rootfs layer: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(reader, maxBlobBytes+1))
	closeErr := reader.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxBlobBytes {
		return fmt.Errorf("uncompressed Docker layer exceeds %d bytes", maxBlobBytes)
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != expectedDiffID {
		return fmt.Errorf("uncompressed layer diff_id mismatch")
	}
	return nil
}

func writeDockerTarBytes(writer *tar.Writer, name string, data []byte, modTime time.Time) error {
	header := &tar.Header{
		Name:     name,
		Mode:     0600,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  modTime,
		Format:   tar.FormatUSTAR,
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func writeDockerTarDirectory(writer *tar.Writer, name string, modTime time.Time) error {
	header := &tar.Header{
		Name:     name,
		Mode:     0755,
		Typeflag: tar.TypeDir,
		ModTime:  modTime,
		Format:   tar.FormatUSTAR,
	}
	return writer.WriteHeader(header)
}

func writeDockerTarFile(writer *tar.Writer, name, source string, size int64, modTime time.Time) error {
	header := &tar.Header{
		Name:     name,
		Mode:     0644,
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  modTime,
		Format:   tar.FormatUSTAR,
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	_, err = io.CopyN(writer, input, size)
	return err
}
