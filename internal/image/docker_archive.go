package image

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	maxDockerArchiveEntries = int64(100000)
	maxDockerArchiveBytes   = int64(16 << 30)
	maxDockerLayerCount     = 128
	maxDockerImageCount     = 128
)

type dockerArchiveReader struct {
	io.Reader
	closeFn func() error
}

func (r *dockerArchiveReader) Close() error {
	if r == nil || r.closeFn == nil {
		return nil
	}
	return r.closeFn()
}

func openDockerArchive(pathname string) (*dockerArchiveReader, error) {
	file, err := os.Open(pathname)
	if err != nil {
		return nil, fmt.Errorf("open Docker archive: %w", err)
	}
	return openDockerArchiveFile(file)
}

func openDockerArchiveFile(file *os.File) (*dockerArchiveReader, error) {
	buffered := bufio.NewReader(file)
	magic, peekErr := buffered.Peek(2)
	if peekErr == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		compressed, gzipErr := gzip.NewReader(buffered)
		if gzipErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("open compressed Docker archive: %w", gzipErr)
		}
		return &dockerArchiveReader{
			Reader: compressed,
			closeFn: func() error {
				return errors.Join(compressed.Close(), file.Close())
			},
		}, nil
	}
	if peekErr == nil && magic[0] == 0x28 {
		frameHeader, headerErr := buffered.Peek(4)
		if headerErr == nil && bytes.Equal(frameHeader, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
			compressed, zstdErr := zstd.NewReader(
				buffered,
				zstd.WithDecoderMaxMemory(256<<20),
				zstd.WithDecoderMaxWindow(256<<20),
				zstd.WithDecoderConcurrency(1),
			)
			if zstdErr != nil {
				_ = file.Close()
				return nil, fmt.Errorf("open Zstandard Docker archive: %w", zstdErr)
			}
			return &dockerArchiveReader{
				Reader: compressed,
				closeFn: func() error {
					compressed.Close()
					return file.Close()
				},
			}, nil
		}
	}
	return &dockerArchiveReader{Reader: buffered, closeFn: file.Close}, nil
}

// isDockerSaveArchive recognizes the standard Docker save layout without
// consuming the archive. Docker writes manifest.json as its first root entry;
// Boxr's own OCI archive starts with the blobs directory.
func isDockerSaveArchive(pathname string) bool {
	input, err := openDockerArchive(pathname)
	if err != nil {
		return false
	}
	defer input.Close()
	reader := tar.NewReader(input)
	for range 8 {
		header, err := reader.Next()
		if err != nil {
			return false
		}
		clean, err := cleanDockerArchivePath(header.Name)
		if err != nil {
			return false
		}
		if clean == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		return clean == "manifest.json" && (header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA)
	}
	return false
}

type dockerManifestEntry struct {
	Config   string
	RepoTags []string
	Layers   []string
}

type dockerImageConfig struct {
	Created      string
	Architecture string
	OS           string
	Variant      string
	Config       ImageRuntimeConfig
	RootFS       struct {
		Type    string
		DiffIDs []string
	}
}

type stagedDockerImage struct {
	root   string
	config dockerImageConfig
	tags   []string
}

// LoadDockerArchive imports the output of "docker save". Every archive path,
// image config and uncompressed layer digest is validated before any image tag
// is published to the local store. Layers are applied in Docker order, then
// flattened into Boxr's native single-rootfs-layer image representation.
func LoadDockerArchive(archivePath string) ([]Image, error) {
	stageParent, err := os.MkdirTemp("", "boxr-docker-save-*")
	if err != nil {
		return nil, fmt.Errorf("create Docker import workspace: %w", err)
	}
	defer os.RemoveAll(stageParent)

	stage := filepath.Join(stageParent, "archive")
	if err := os.Mkdir(stage, 0700); err != nil {
		return nil, err
	}
	if err := extractDockerArchive(archivePath, stage); err != nil {
		return nil, err
	}

	stageRoot, err := os.OpenRoot(stage)
	if err != nil {
		return nil, fmt.Errorf("open Docker archive staging root: %w", err)
	}
	defer stageRoot.Close()
	manifestData, err := readDockerJSON(stageRoot, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("read Docker manifest: %w", err)
	}
	var entries []dockerManifestEntry
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode Docker manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("Docker manifest contains trailing JSON data")
	}
	if len(entries) == 0 || len(entries) > maxDockerImageCount {
		return nil, fmt.Errorf("Docker manifest must contain between 1 and %d images", maxDockerImageCount)
	}

	staged := make([]stagedDockerImage, 0, len(entries))
	seenTags := make(map[string]bool)
	totalLayers := 0
	for imageIndex, entry := range entries {
		if len(entry.RepoTags) == 0 {
			return nil, fmt.Errorf("Docker image %d has no RepoTags", imageIndex+1)
		}
		if entry.Config == "" || len(entry.Layers) == 0 || len(entry.Layers) > maxDockerLayerCount {
			return nil, fmt.Errorf("Docker image %d has an invalid config or layer list", imageIndex+1)
		}
		totalLayers += len(entry.Layers)
		if totalLayers > maxDockerImageCount*maxDockerLayerCount {
			return nil, fmt.Errorf("Docker archive contains too many layers")
		}
		tags := make([]string, 0, len(entry.RepoTags))
		for _, tag := range entry.RepoTags {
			if err := ValidateReference(tag); err != nil {
				return nil, fmt.Errorf("Docker image %d has invalid repository tag: %w", imageIndex+1, err)
			}
			if seenTags[tag] {
				return nil, fmt.Errorf("Docker archive contains duplicate repository tag %q", tag)
			}
			seenTags[tag] = true
			tags = append(tags, tag)
		}

		configName, err := cleanDockerArchivePath(entry.Config)
		if err != nil {
			return nil, fmt.Errorf("Docker image %d config path: %w", imageIndex+1, err)
		}
		configData, err := readDockerJSON(stageRoot, configName)
		if err != nil {
			return nil, fmt.Errorf("Docker image %d config: %w", imageIndex+1, err)
		}
		config, err := decodeDockerImageConfig(configData)
		if err != nil {
			return nil, fmt.Errorf("decode Docker image %d config: %w", imageIndex+1, err)
		}
		if config.OS != "linux" || config.Architecture == "" ||
			strings.ContainsAny(config.OS+config.Architecture+config.Variant, "/\\\x00\r\n") {
			return nil, fmt.Errorf("Docker image %d has unsupported platform %q/%q", imageIndex+1, config.OS, config.Architecture)
		}
		if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != len(entry.Layers) {
			return nil, fmt.Errorf("Docker image %d has an invalid rootfs diff_id list", imageIndex+1)
		}
		if err := ValidateBuildSpec(BuildSpec{Version: 1, Base: "scratch", Config: config.Config}); err != nil {
			return nil, fmt.Errorf("Docker image %d has invalid runtime config: %w", imageIndex+1, err)
		}

		root := filepath.Join(stageParent, fmt.Sprintf("rootfs-%03d", imageIndex+1))
		if err := os.Mkdir(root, 0700); err != nil {
			return nil, err
		}
		for layerIndex, layerName := range entry.Layers {
			cleanLayer, err := cleanDockerArchivePath(layerName)
			if err != nil {
				return nil, fmt.Errorf("Docker image %d layer %d path: %w", imageIndex+1, layerIndex+1, err)
			}
			layerPath := filepath.FromSlash(cleanLayer)
			info, err := stageRoot.Lstat(layerPath)
			if err != nil {
				return nil, fmt.Errorf("Docker image %d layer %d: %w", imageIndex+1, layerIndex+1, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxBlobBytes {
				return nil, fmt.Errorf("Docker image %d layer %d is not a bounded regular file", imageIndex+1, layerIndex+1)
			}
			expected := config.RootFS.DiffIDs[layerIndex]
			if !validDockerDiffID(expected) {
				return nil, fmt.Errorf("Docker image %d layer %d has an invalid diff_id", imageIndex+1, layerIndex+1)
			}
			layerFile, err := stageRoot.Open(layerPath)
			if err != nil {
				return nil, fmt.Errorf("Docker image %d layer %d open: %w", imageIndex+1, layerIndex+1, err)
			}
			actual, err := dockerLayerDiffID(layerFile)
			if err != nil {
				return nil, fmt.Errorf("Docker image %d layer %d digest: %w", imageIndex+1, layerIndex+1, err)
			}
			if actual != expected {
				return nil, fmt.Errorf("Docker image %d layer %d diff_id mismatch", imageIndex+1, layerIndex+1)
			}
			if err := applyDockerLayer(root, layerPath); err != nil {
				return nil, fmt.Errorf("apply Docker image %d layer %d: %w", imageIndex+1, layerIndex+1, err)
			}
		}
		staged = append(staged, stagedDockerImage{root: root, config: config, tags: tags})
	}

	// Do not publish anything until the complete manifest and every image layer
	// have been decoded, digest-checked and applied successfully.
	results := make([]Image, 0, len(seenTags))
	for _, item := range staged {
		for _, tag := range item.tags {
			created, err := ImportRootFSWithConfig(
				item.root, tag, item.config.OS, item.config.Architecture,
				item.config.Variant, item.config.Config,
			)
			if err != nil {
				return nil, fmt.Errorf("import Docker image tag %q: %w", tag, err)
			}
			results = append(results, created)
		}
	}
	return results, nil
}

func decodeDockerImageConfig(data []byte) (dockerImageConfig, error) {
	var config dockerImageConfig
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return config, err
	}
	for key, target := range map[string]any{
		"created": &config.Created, "architecture": &config.Architecture,
		"os": &config.OS, "variant": &config.Variant, "config": &config.Config,
	} {
		if raw, ok := fields[key]; ok {
			if err := json.Unmarshal(raw, target); err != nil {
				return config, fmt.Errorf("invalid %s field: %w", key, err)
			}
		}
	}
	rootFSRaw, ok := fields["rootfs"]
	if !ok {
		return config, fmt.Errorf("missing rootfs object")
	}
	var rootFSFields map[string]json.RawMessage
	if err := json.Unmarshal(rootFSRaw, &rootFSFields); err != nil {
		return config, fmt.Errorf("invalid rootfs object: %w", err)
	}
	if raw, ok := rootFSFields["type"]; ok {
		if err := json.Unmarshal(raw, &config.RootFS.Type); err != nil {
			return config, fmt.Errorf("invalid rootfs type: %w", err)
		}
	}
	if raw, ok := rootFSFields["diff_ids"]; ok {
		if err := json.Unmarshal(raw, &config.RootFS.DiffIDs); err != nil {
			return config, fmt.Errorf("invalid rootfs diff_ids: %w", err)
		}
	}
	return config, nil
}

func extractDockerArchive(archivePath, destination string) error {
	input, err := openDockerArchive(archivePath)
	if err != nil {
		return err
	}
	defer input.Close()

	archiveRoot, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open Docker archive extraction root: %w", err)
	}
	defer archiveRoot.Close()

	reader := tar.NewReader(input)
	seen := make(map[string]byte)
	var entries, totalBytes int64
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("read Docker archive entry: %w", nextErr)
		}
		entries++
		if entries > maxDockerArchiveEntries {
			return fmt.Errorf("Docker archive exceeds %d entries", maxDockerArchiveEntries)
		}
		if header.Size < 0 || header.Size > maxDockerArchiveBytes-totalBytes {
			return fmt.Errorf("Docker archive exceeds the %d-byte uncompressed limit", maxDockerArchiveBytes)
		}
		totalBytes += header.Size

		name, err := cleanDockerArchivePath(header.Name)
		if err != nil {
			return fmt.Errorf("unsafe Docker archive path %q: %w", header.Name, err)
		}
		if name == "." {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("Docker archive root entry must be a directory")
			}
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := checkDockerEntryCollision(seen, name, true); err != nil {
				return err
			}
			if previous, ok := seen[name]; ok && previous != tar.TypeDir {
				return fmt.Errorf("Docker archive path %q has conflicting entries", name)
			}
			seen[name] = tar.TypeDir
			target, err := dockerTarget(archiveRoot, name, true)
			if err != nil {
				return err
			}
			if info, statErr := archiveRoot.Lstat(target); statErr == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return fmt.Errorf("Docker archive directory %q conflicts with an existing entry", name)
				}
			} else if errors.Is(statErr, os.ErrNotExist) {
				if err := archiveRoot.Mkdir(target, 0700); err != nil {
					return err
				}
			} else {
				return statErr
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := checkDockerEntryCollision(seen, name, false); err != nil {
				return err
			}
			if _, ok := seen[name]; ok {
				return fmt.Errorf("Docker archive contains duplicate path %q", name)
			}
			seen[name] = tar.TypeReg
			target, err := dockerTarget(archiveRoot, name, true)
			if err != nil {
				return err
			}
			file, err := archiveRoot.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return fmt.Errorf("create Docker archive entry %q: %w", name, err)
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract Docker archive entry %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close Docker archive entry %q: %w", name, closeErr)
			}
		default:
			return fmt.Errorf("Docker archive contains unsupported entry type %d at %q", header.Typeflag, name)
		}
	}
	if _, ok := seen["manifest.json"]; !ok {
		return fmt.Errorf("archive is not a Docker save archive: manifest.json is missing")
	}
	return nil
}

func cleanDockerArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute, empty or invalid path")
	}
	for _, component := range strings.Split(strings.TrimPrefix(name, "./"), "/") {
		if component == ".." {
			return "", fmt.Errorf("path contains parent traversal")
		}
	}
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." {
		return clean, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes archive root")
	}
	for _, component := range strings.Split(clean, "/") {
		if component == "" || component == "." || component == ".." || strings.Contains(component, ":") {
			return "", fmt.Errorf("invalid path component")
		}
	}
	return clean, nil
}

func checkDockerEntryCollision(seen map[string]byte, name string, directory bool) error {
	for parent := path.Dir(name); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if kind, ok := seen[parent]; ok && kind != tar.TypeDir {
			return fmt.Errorf("archive path %q has a non-directory parent %q", name, parent)
		}
	}
	if !directory {
		prefix := name + "/"
		for previous := range seen {
			if strings.HasPrefix(previous, prefix) {
				return fmt.Errorf("archive file %q conflicts with child path %q", name, previous)
			}
		}
	}
	return nil
}

func dockerTarget(root *os.Root, name string, createParents bool) (string, error) {
	clean, err := cleanDockerArchivePath(name)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return ".", nil
	}
	relative := filepath.FromSlash(clean)
	parts := strings.Split(relative, string(filepath.Separator))
	for index := 1; index < len(parts); index++ {
		parent := strings.Join(parts[:index], string(filepath.Separator))
		info, statErr := root.Lstat(parent)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			if !createParents {
				return relative, nil
			}
			if err := root.Mkdir(parent, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
		case statErr != nil:
			return "", statErr
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			if !createParents {
				return "", fmt.Errorf("path parent %q is not a real directory", parent)
			}
			if err := root.RemoveAll(parent); err != nil {
				return "", err
			}
			if err := root.Mkdir(parent, 0755); err != nil {
				return "", err
			}
		}
	}
	return relative, nil
}

func dockerEnsureDirectory(root *os.Root, name string, mode int64) error {
	if name == "." {
		return nil
	}
	target, err := dockerTarget(root, name, true)
	if err != nil {
		return err
	}
	info, err := root.Lstat(target)
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		if err := root.RemoveAll(target); err != nil {
			return err
		}
		err = os.ErrNotExist
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := root.Mkdir(target, safeDockerMode(mode, true)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return root.Chmod(target, safeDockerMode(mode, true))
}

func dockerRemovePath(root *os.Root, name string) error {
	if name == "." {
		return fmt.Errorf("refusing to whiteout the root filesystem")
	}
	target, err := dockerTarget(root, name, false)
	if err != nil {
		return err
	}
	if err := root.RemoveAll(target); err != nil {
		return fmt.Errorf("apply Docker whiteout %q: %w", name, err)
	}
	return nil
}

func dockerClearDirectory(root *os.Root, name string) error {
	if err := dockerEnsureDirectory(root, name, 0755); err != nil {
		return err
	}
	target := filepath.FromSlash(name)
	if name == "." {
		target = "."
	} else {
		var targetErr error
		target, targetErr = dockerTarget(root, name, true)
		if targetErr != nil {
			return targetErr
		}
	}
	directory, err := root.Open(target)
	if err != nil {
		return err
	}
	children, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, child := range children {
		childPath := child.Name()
		if target != "." {
			childPath = target + string(filepath.Separator) + child.Name()
		}
		if err := root.RemoveAll(childPath); err != nil {
			return err
		}
	}
	return nil
}

func validDockerDiffID(value string) bool {
	algorithm, digest, ok := strings.Cut(value, ":")
	return ok && algorithm == "sha256" && hexRE.MatchString(digest)
}

func dockerLayerDiffID(file *os.File) (string, error) {
	input, err := openDockerArchiveFile(file)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(input, maxBlobBytes+1))
	if err != nil {
		return "", err
	}
	if written > maxBlobBytes {
		return "", fmt.Errorf("uncompressed layer exceeds %d bytes", maxBlobBytes)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func validateDockerSymlink(name, target string) error {
	if target == "" || strings.ContainsAny(target, "\\\x00") {
		return fmt.Errorf("empty or invalid symlink target")
	}
	if path.IsAbs(target) {
		return nil // absolute paths are interpreted inside the container root
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink target escapes root filesystem")
	}
	return nil
}

func safeDockerMode(mode int64, directory bool) os.FileMode {
	permissions := os.FileMode(mode) & 0777
	if permissions == 0 {
		if directory {
			return 0755
		}
		return 0644
	}
	return permissions
}

func scanDockerWhiteouts(layerPath string) (map[string]bool, map[string]bool, error) {
	whiteouts := make(map[string]bool)
	opaque := make(map[string]bool)
	input, err := openDockerArchive(layerPath)
	if err != nil {
		return nil, nil, err
	}
	defer input.Close()
	reader := tar.NewReader(input)
	var count int64
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, nil, nextErr
		}
		count++
		if count > maxDockerArchiveEntries {
			return nil, nil, fmt.Errorf("Docker layer exceeds %d entries", maxDockerArchiveEntries)
		}
		name, err := cleanDockerArchivePath(header.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("unsafe layer path %q: %w", header.Name, err)
		}
		if name == "." {
			continue
		}
		base := path.Base(name)
		if base != ".wh..wh..opq" && !strings.HasPrefix(base, ".wh.") {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size != 0 {
			return nil, nil, fmt.Errorf("Docker whiteout %q must be an empty regular file", name)
		}
		if base == ".wh..wh..opq" {
			directory := path.Dir(name)
			opaque[directory] = true
			continue
		}
		targetName := strings.TrimPrefix(base, ".wh.")
		if targetName == "" {
			return nil, nil, fmt.Errorf("invalid Docker whiteout %q", name)
		}
		target := targetName
		if directory := path.Dir(name); directory != "." {
			target = directory + "/" + targetName
		}
		target, err = cleanDockerArchivePath(target)
		if err != nil || target == "." {
			return nil, nil, fmt.Errorf("invalid Docker whiteout target for %q", name)
		}
		whiteouts[target] = true
	}
	return whiteouts, opaque, nil
}

func applyDockerLayer(rootPath, layerPath string) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open layer rootfs: %w", err)
	}
	defer root.Close()

	whiteouts, opaque, err := scanDockerWhiteouts(layerPath)
	if err != nil {
		return fmt.Errorf("scan layer whiteouts: %w", err)
	}
	for directory := range opaque {
		if err := dockerClearDirectory(root, directory); err != nil {
			return fmt.Errorf("clear opaque directory %q: %w", directory, err)
		}
	}
	for target := range whiteouts {
		if err := dockerRemovePath(root, target); err != nil {
			return err
		}
	}

	input, err := openDockerArchive(layerPath)
	if err != nil {
		return err
	}
	defer input.Close()
	reader := tar.NewReader(input)
	seen := make(map[string]byte)
	var count, totalBytes int64
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("read Docker layer: %w", nextErr)
		}
		count++
		if count > maxDockerArchiveEntries {
			return fmt.Errorf("Docker layer exceeds %d entries", maxDockerArchiveEntries)
		}
		if header.Size < 0 || header.Size > maxBlobBytes-totalBytes {
			return fmt.Errorf("Docker layer exceeds %d uncompressed bytes", maxBlobBytes)
		}
		totalBytes += header.Size
		name, err := cleanDockerArchivePath(header.Name)
		if err != nil {
			return fmt.Errorf("unsafe layer path %q: %w", header.Name, err)
		}
		if name == "." {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("layer root entry must be a directory")
			}
			continue
		}
		base := path.Base(name)
		if base == ".wh..wh..opq" || strings.HasPrefix(base, ".wh.") {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := checkDockerEntryCollision(seen, name, true); err != nil {
				return err
			}
			if previous, ok := seen[name]; ok && previous != tar.TypeDir {
				return fmt.Errorf("layer path %q has conflicting entries", name)
			}
			seen[name] = tar.TypeDir
			if err := dockerEnsureDirectory(root, name, header.Mode); err != nil {
				return fmt.Errorf("create layer directory %q: %w", name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := checkDockerEntryCollision(seen, name, false); err != nil {
				return err
			}
			if _, ok := seen[name]; ok {
				return fmt.Errorf("layer contains duplicate path %q", name)
			}
			seen[name] = tar.TypeReg
			target, err := dockerTarget(root, name, true)
			if err != nil {
				return err
			}
			if err := root.RemoveAll(target); err != nil {
				return err
			}
			file, err := root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, safeDockerMode(header.Mode, false))
			if err != nil {
				return fmt.Errorf("create layer file %q: %w", name, err)
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract layer file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
			if err := root.Chmod(target, safeDockerMode(header.Mode, false)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := checkDockerEntryCollision(seen, name, false); err != nil {
				return err
			}
			if _, ok := seen[name]; ok {
				return fmt.Errorf("layer contains duplicate path %q", name)
			}
			seen[name] = tar.TypeSymlink
			if err := validateDockerSymlink(name, header.Linkname); err != nil {
				return fmt.Errorf("unsafe layer symlink %q: %w", name, err)
			}
			target, err := dockerTarget(root, name, true)
			if err != nil {
				return err
			}
			if err := root.RemoveAll(target); err != nil {
				return err
			}
			if err := root.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("create layer symlink %q: %w", name, err)
			}
		case tar.TypeLink:
			if err := checkDockerEntryCollision(seen, name, false); err != nil {
				return err
			}
			if _, ok := seen[name]; ok {
				return fmt.Errorf("layer contains duplicate path %q", name)
			}
			seen[name] = tar.TypeLink
			linkName, err := cleanDockerArchivePath(header.Linkname)
			if err != nil || linkName == "." || linkName == name {
				return fmt.Errorf("unsafe hard link target %q", header.Linkname)
			}
			source, err := dockerTarget(root, linkName, true)
			if err != nil {
				return err
			}
			info, err := root.Lstat(source)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("hard link target %q must be an existing regular file", linkName)
			}
			target, err := dockerTarget(root, name, true)
			if err != nil {
				return err
			}
			if err := root.RemoveAll(target); err != nil {
				return err
			}
			if err := root.Link(source, target); err != nil {
				return fmt.Errorf("create layer hard link %q: %w", name, err)
			}
		default:
			return fmt.Errorf("unsupported special layer entry type %d at %q", header.Typeflag, name)
		}
	}
	return nil
}

// ApplyLayerArchive safely applies one OCI/Docker-compatible tar or gzip-tar
// layer to an existing private rootfs directory. Layer ordering is the caller's
// responsibility; path traversal, escaping symlinks, unsafe hard links, special
// files, duplicate entries and archive size limits are enforced here.
func ApplyLayerArchive(root, archivePath string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect layer rootfs: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("layer rootfs must be a real directory, not a symlink")
	}
	layerInfo, err := os.Lstat(archivePath)
	if err != nil {
		return fmt.Errorf("inspect layer archive: %w", err)
	}
	if layerInfo.Mode()&os.ModeSymlink != 0 || !layerInfo.Mode().IsRegular() || layerInfo.Size() < 0 || layerInfo.Size() > maxBlobBytes {
		return fmt.Errorf("layer archive must be a regular file no larger than %d bytes", maxBlobBytes)
	}
	return applyDockerLayer(root, archivePath)
}

func readDockerJSON(root *os.Root, name string) ([]byte, error) {
	clean, err := cleanDockerArchivePath(name)
	if err != nil {
		return nil, err
	}
	relative := filepath.FromSlash(clean)
	info, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxJSONBytes {
		return nil, fmt.Errorf("JSON file must be a regular non-symlink file no larger than %d bytes", maxJSONBytes)
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxJSONBytes {
		return nil, fmt.Errorf("JSON file exceeds %d bytes", maxJSONBytes)
	}
	return data, nil
}


	info, err := os.Lstat(pathname)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxJSONBytes {
		return nil, fmt.Errorf("JSON file must be a regular non-symlink file no larger than %d bytes", maxJSONBytes)
	}
	file, err := os.Open(pathname)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxJSONBytes {
		return nil, fmt.Errorf("JSON file exceeds %d bytes", maxJSONBytes)
	}
	return data, nil
}
