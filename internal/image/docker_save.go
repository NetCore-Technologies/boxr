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
func SaveDocker(reference, archivePath string) error {
	if err := ValidateReference(reference); err != nil {
		return err
	}
	if archivePath == "" {
		return fmt.Errorf("Docker archive path must not be empty")
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
	destination, err := filepath.Abs(archivePath)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(storeAbs, destination)
	if err != nil {
		return err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return fmt.Errorf("Docker archive destination must be outside the image store")
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
		return fmt.Errorf("verify image before Docker save: %w", err)
	}
	manifest, err := loadManifest(dir, *selected)
	if err != nil {
		return err
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != layerType {
		return fmt.Errorf("image %q has an unsupported layer layout", reference)
	}
	configData, err := readBlob(dir, manifest.Config)
	if err != nil {
		return err
	}
	var config ImageConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}
	if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != 1 ||
		!validDockerDiffID(config.RootFS.DiffIDs[0]) {
		return fmt.Errorf("image %q has an invalid rootfs diff_id", reference)
	}

	layerPath, err := verifyBlob(dir, manifest.Layers[0])
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "boxr-docker-save-*")
	if err != nil {
		return fmt.Errorf("create Docker save workspace: %w", err)
	}
	defer os.RemoveAll(stage)

	uncompressedLayer := filepath.Join(stage, "layer.tar")
	layerFile, err := os.OpenFile(uncompressedLayer, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := decompressDockerLayer(layerPath, layerFile, config.RootFS.DiffIDs[0]); err != nil {
		_ = layerFile.Close()
		return fmt.Errorf("prepare Docker layer: %w", err)
	}
	layerInfo, err := layerFile.Stat()
	if err != nil {
		_ = layerFile.Close()
		return err
	}
	if err := layerFile.Close(); err != nil {
		return err
	}

	dockerConfig := dockerSaveConfig{
		ImageConfig: config,
		History: []dockerSaveHistory{{
			Created:   config.Created,
			CreatedBy: "boxr image save --format docker",
		}},
	}
	configBytes, err := json.Marshal(dockerConfig)
	if err != nil {
		return err
	}
	configDigest := digestBytes(configBytes)
	configName := strings.TrimPrefix(configDigest, "sha256:") + ".json"
	layerID := strings.TrimPrefix(config.RootFS.DiffIDs[0], "sha256:")
	layerName := layerID + "/layer.tar"

	repository, tag, err := splitDockerRepositoryTag(reference)
	if err != nil {
		return err
	}
	manifestBytes, err := json.Marshal([]dockerSaveManifestEntry{{
		Config:   configName,
		RepoTags: []string{reference},
		Layers:   []string{layerName},
	}})
	if err != nil {
		return err
	}
	repositoriesBytes, err := json.Marshal(map[string]map[string]string{
		repository: {tag: layerID},
	})
	if err != nil {
		return err
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve Docker archive destination directory: %w", err)
	}
	resolvedRelative, err := filepath.Rel(storeAbs, resolvedParent)
	if err != nil {
		return err
	}
	if resolvedRelative == "." || (resolvedRelative != ".." && !strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator))) {
		return fmt.Errorf("Docker archive destination resolves inside the image store")
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
	if err := writeDockerTarBytes(writer, "manifest.json", manifestBytes, archiveTime); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writeDockerTarBytes(writer, configName, configBytes, archiveTime); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writeDockerTarBytes(writer, "repositories", repositoriesBytes, archiveTime); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writeDockerTarDirectory(writer, layerID+"/", archiveTime); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writeDockerTarFile(writer, layerName, uncompressedLayer, layerInfo.Size(), archiveTime); err != nil {
		_ = writer.Close()
		return err
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
