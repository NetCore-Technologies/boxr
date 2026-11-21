// Package volume manages persistent, owner-private directories mounted into
// Boxr containers as bind mounts.
package volume

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/rootfs"
)

var mu sync.Mutex

const maxMetadataBytes = 1 << 20

type Volume struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	Anonymous bool              `json:"anonymous,omitempty"`
}

// Usage is a no-follow filesystem snapshot of a volume's logical contents.
type Usage struct {
	Name         string `json:"name"`
	Files        int64  `json:"files"`
	Directories  int64  `json:"directories"`
	Symlinks     int64  `json:"symlinks"`
	OtherEntries int64  `json:"otherEntries"`
	FileBytes    int64  `json:"fileBytes"`
	SymlinkBytes int64  `json:"symlinkBytes"`
	TotalBytes   int64  `json:"totalBytes"`
	Snapshot     bool   `json:"snapshot"`
}

// Archive describes a portable, gzip-compressed volume export.
type Archive struct {
	Volume    string          `json:"volume"`
	Path      string          `json:"path"`
	Digest    string          `json:"digest"`
	SizeBytes int64           `json:"sizeBytes"`
	Manifest  rootfs.Manifest `json:"manifest"`
}

func DataDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("BOXR_DATA_DIR")); value != "" {
		return filepath.Abs(value)
	}
	if value := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); value != "" {
		return filepath.Join(value, "boxr"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "boxr"), nil
}

func dirs() (string, string, error) {
	base, err := DataDir()
	if err != nil {
		return "", "", err
	}
	volumes := filepath.Join(base, "volumes")
	metadata := filepath.Join(base, "volume-metadata")
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", "", fmt.Errorf("create Boxr data directory %s: %w", base, err)
	}
	baseInfo, err := os.Lstat(base)
	if err != nil {
		return "", "", err
	}
	if baseInfo.Mode()&os.ModeSymlink != 0 || !baseInfo.IsDir() {
		return "", "", fmt.Errorf("Boxr data path %s must be a real directory", base)
	}
	for _, dir := range []string{volumes, metadata} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", "", fmt.Errorf("create volume directory %s: %w", dir, err)
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", "", fmt.Errorf("volume path %s must be a real directory", dir)
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return "", "", fmt.Errorf("secure volume directory %s: %w", dir, err)
		}
	}
	return volumes, metadata, nil
}

func Create(name string, labels map[string]string) (Volume, error) {
	return create(name, labels, false)
}

// CreateAnonymous creates a Docker-compatible anonymous local volume using a
// cryptographically random name. Anonymous identity is persisted so Engine API
// volume prune can distinguish generated volumes from explicitly named ones.
func CreateAnonymous(labels map[string]string) (Volume, error) {
	var identifier [32]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return Volume{}, fmt.Errorf("generate anonymous volume name: %w", err)
	}
	return create(hex.EncodeToString(identifier[:]), labels, true)
}

func create(name string, labels map[string]string, anonymous bool) (Volume, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := validateName(name); err != nil {
		return Volume{}, err
	}
	if err := validateLabels(labels); err != nil {
		return Volume{}, err
	}
	volumes, metadata, err := dirs()
	if err != nil {
		return Volume{}, err
	}
	path := filepath.Join(volumes, name)
	if _, err := os.Lstat(path); err == nil {
		return Volume{}, fmt.Errorf("volume %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Volume{}, err
	}
	metaPath := filepath.Join(metadata, name+".json")
	if _, err := os.Lstat(metaPath); err == nil {
		return Volume{}, fmt.Errorf("volume metadata for %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Volume{}, err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return Volume{}, fmt.Errorf("create volume %q: %w", name, err)
	}
	item := Volume{
		Name: name, Path: path, Labels: cloneLabels(labels),
		CreatedAt: time.Now().UTC(), Anonymous: anonymous,
	}
	if err := saveMetadata(metaPath, item); err != nil {
		_ = os.Remove(path)
		return Volume{}, err
	}
	return item, nil
}

// ImportArchive safely restores a gzip-tar volume export. Extraction is
// staged beside the destination and only published after the archive passes
// the rootfs extractor's traversal, symlink, duplicate-path and size checks.
func ImportArchive(name, archivePath string, labels map[string]string) (Volume, rootfs.Manifest, error) {
	mu.Lock()
	defer mu.Unlock()
	var empty rootfs.Manifest
	if err := validateName(name); err != nil {
		return Volume{}, empty, err
	}
	if err := validateLabels(labels); err != nil {
		return Volume{}, empty, err
	}
	volumes, metadata, err := dirs()
	if err != nil {
		return Volume{}, empty, err
	}
	path := filepath.Join(volumes, name)
	if _, err := os.Lstat(path); err == nil {
		return Volume{}, empty, fmt.Errorf("volume %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Volume{}, empty, err
	}
	metaPath := filepath.Join(metadata, name+".json")
	if _, err := os.Lstat(metaPath); err == nil {
		return Volume{}, empty, fmt.Errorf("volume metadata for %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Volume{}, empty, err
	}
	stage, err := os.MkdirTemp(volumes, ".boxr-volume-import-*")
	if err != nil {
		return Volume{}, empty, err
	}
	if err := os.Remove(stage); err != nil {
		return Volume{}, empty, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
			_ = os.RemoveAll(path)
		}
	}()
	manifest, err := rootfs.ExtractTarGz(archivePath, stage, rootfs.Limits{})
	if err != nil {
		return Volume{}, empty, fmt.Errorf("extract volume archive: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return Volume{}, empty, fmt.Errorf("volume %q was created during import", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Volume{}, empty, err
	}
	if err := os.Rename(stage, path); err != nil {
		return Volume{}, empty, fmt.Errorf("publish imported volume: %w", err)
	}
	item := Volume{Name: name, Path: path, Labels: cloneLabels(labels), CreatedAt: time.Now().UTC()}
	if err := saveMetadata(metaPath, item); err != nil {
		return Volume{}, empty, fmt.Errorf("save imported volume metadata: %w", err)
	}
	committed = true
	return item, manifest, nil
}

// ExportArchive writes a portable gzip-tar snapshot to a new path. The
// destination is published with an atomic no-clobber link, so an existing
// backup is never silently replaced. Callers should prevent active workloads
// from writing the volume while the point-in-time archive is being created.
func ExportArchive(name, archivePath string) (Archive, error) {
	mu.Lock()
	defer mu.Unlock()

	var result Archive
	item, err := GetUnlocked(name)
	if err != nil {
		return result, err
	}
	absolute, err := filepath.Abs(archivePath)
	if err != nil {
		return result, fmt.Errorf("resolve archive destination: %w", err)
	}
	filename := filepath.Base(absolute)
	if filename == "." || filename == string(filepath.Separator) || filename == "" {
		return result, fmt.Errorf("archive destination must name a file")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return result, fmt.Errorf("resolve archive directory: %w", err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return result, fmt.Errorf("archive parent must be an existing directory")
	}
	destination := filepath.Join(parent, filename)
	if _, err := os.Lstat(destination); err == nil {
		return result, fmt.Errorf("archive destination %s already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("inspect archive destination: %w", err)
	}

	volumeRoot, err := filepath.EvalSymlinks(item.Path)
	if err != nil {
		return result, fmt.Errorf("resolve volume data path: %w", err)
	}
	relative, err := filepath.Rel(volumeRoot, destination)
	if err != nil {
		return result, err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return result, fmt.Errorf("archive destination must be outside the source volume")
	}

	temporary, err := os.CreateTemp(parent, ".boxr-volume-export-*.tar.gz")
	if err != nil {
		return result, fmt.Errorf("create archive staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return result, err
	}
	defer os.Remove(temporaryPath)

	manifest, err := rootfs.PackTarGz(item.Path, temporaryPath)
	if err != nil {
		return result, fmt.Errorf("pack volume %s: %w", name, err)
	}
	archive, err := os.Open(temporaryPath)
	if err != nil {
		return result, fmt.Errorf("open completed volume archive: %w", err)
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(hasher, archive)
	closeErr := archive.Close()
	if copyErr != nil {
		return result, fmt.Errorf("digest volume archive: %w", copyErr)
	}
	if closeErr != nil {
		return result, fmt.Errorf("close volume archive: %w", closeErr)
	}
	if err := os.Link(temporaryPath, destination); err != nil {
		return result, fmt.Errorf("publish archive without replacing an existing file: %w", err)
	}
	if directory, err := os.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return Archive{
		Volume:    name,
		Path:      destination,
		Digest:    "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
		SizeBytes: size,
		Manifest:  manifest,
	}, nil
}



type copyUpDirectoryMode struct {
	path string
	mode os.FileMode
}

// copyUpRootFSTree copies a verified image subtree without following symlinks.
// Symlink targets are preserved byte-for-byte: after the volume is mounted,
// absolute and relative targets must retain their container-root semantics.
// Special files are rejected, and traversal is bounded before data is copied.
func copyUpRootFSTree(source, destination string) (int64, int64, error) {
	var entries, totalBytes int64
	var directories []copyUpDirectoryMode

	err := filepath.WalkDir(source, func(sourcePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if sourcePath == source {
			return nil
		}
		entries++
		if entries > rootfs.DefaultMaxFiles {
			return fmt.Errorf("image mount source exceeds the %d-entry copy-up limit", rootfs.DefaultMaxFiles)
		}

		relative, err := filepath.Rel(source, sourcePath)
		if err != nil {
			return fmt.Errorf("resolve image mount source entry: %w", err)
		}
		if relative == "." || !filepath.IsLocal(relative) {
			return fmt.Errorf("image mount source contains a non-local path %q", relative)
		}
		targetPath := filepath.Join(destination, relative)
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return fmt.Errorf("inspect image mount source entry %s: %w", relative, err)
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(sourcePath)
			if err != nil {
				return fmt.Errorf("read image symlink %s: %w", relative, err)
			}
			if err := os.Symlink(linkTarget, targetPath); err != nil {
				return fmt.Errorf("copy image symlink %s: %w", relative, err)
			}
		case info.IsDir():
			if err := os.Mkdir(targetPath, 0700); err != nil {
				return fmt.Errorf("stage image directory %s: %w", relative, err)
			}
			directories = append(directories, copyUpDirectoryMode{path: targetPath, mode: info.Mode().Perm()})
		case info.Mode().IsRegular():
			if info.Size() < 0 || info.Size() > rootfs.DefaultMaxBytes-totalBytes {
				return fmt.Errorf("image mount source exceeds the %d-byte copy-up limit", rootfs.DefaultMaxBytes)
			}
			input, err := os.Open(sourcePath)
			if err != nil {
				return fmt.Errorf("open image file %s: %w", relative, err)
			}
			openedInfo, statErr := input.Stat()
			if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
				closeErr := input.Close()
				return errors.Join(
					fmt.Errorf("image file %s changed while preparing volume copy-up", relative),
					statErr,
					closeErr,
				)
			}
			output, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				closeErr := input.Close()
				return errors.Join(fmt.Errorf("create staged image file %s: %w", relative, err), closeErr)
			}
			written, copyErr := io.CopyN(output, input, info.Size())
			syncErr := output.Sync()
			modeErr := output.Chmod(info.Mode().Perm())
			outputCloseErr := output.Close()
			inputCloseErr := input.Close()
			if copyErr != nil || syncErr != nil || modeErr != nil || outputCloseErr != nil || inputCloseErr != nil {
				return errors.Join(
					fmt.Errorf("copy image file %s", relative),
					copyErr,
					syncErr,
					modeErr,
					outputCloseErr,
					inputCloseErr,
				)
			}
			if written != info.Size() {
				return fmt.Errorf("image file %s changed size during volume copy-up", relative)
			}
			totalBytes += written
		default:
			return fmt.Errorf("image mount source contains unsupported special file %s", relative)
		}
		return nil
	})
	if err != nil {
		return entries, totalBytes, err
	}

	// Set directory permissions after populating children, because an image
	// may contain read-only directories that would otherwise block staging.
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		if err := os.Chmod(directory.path, directory.mode); err != nil {
			return entries, totalBytes, fmt.Errorf("restore image directory mode for %s: %w", directory.path, err)
		}
	}
	return entries, totalBytes, nil
}

// CopyUpFromRootFS populates an empty managed volume from a directory in a
 // verified image rootfs. Non-empty volumes are never modified. The source is
 // archived and safely extracted using the rootfs package's path, symlink and
 // size checks before any entries are published into the volume.
func CopyUpFromRootFS(name, rootPath, mountTarget string) error {
	mu.Lock()
	defer mu.Unlock()

	item, err := GetUnlocked(name)
	if err != nil {
		return fmt.Errorf("resolve volume %q: %w", name, err)
	}
	destinationEntries, err := os.ReadDir(item.Path)
	if err != nil {
		return fmt.Errorf("inspect volume %q before copy-up: %w", name, err)
	}
	if len(destinationEntries) != 0 {
		return nil
	}

	source, exists, err := resolveCopyUpSource(rootPath, mountTarget)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	staging, err := os.MkdirTemp(filepath.Dir(item.Path), ".boxr-volume-copy-up-*")
	if err != nil {
		return fmt.Errorf("create volume copy-up staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	extractedPath := filepath.Join(staging, "contents")
	if err := os.Mkdir(extractedPath, 0700); err != nil {
		return fmt.Errorf("create staged volume copy-up tree: %w", err)
	}
	if _, _, err := copyUpRootFSTree(source, extractedPath); err != nil {
		return fmt.Errorf("copy image mount source into staging: %w", err)
	}
	stagedEntries, err := os.ReadDir(extractedPath)
	if err != nil {
		return fmt.Errorf("read staged volume copy-up: %w", err)
	}
	if len(stagedEntries) == 0 {
		return nil
	}

	// Recheck immediately before publication. Another Boxr operation may have
	// populated this volume while the image tree was being staged.
	destinationEntries, err = os.ReadDir(item.Path)
	if err != nil {
		return fmt.Errorf("recheck volume %q before copy-up: %w", name, err)
	}
	if len(destinationEntries) != 0 {
		return nil
	}
	for _, entry := range stagedEntries {
		destination := filepath.Join(item.Path, entry.Name())
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("volume %q became non-empty during copy-up", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect volume copy-up destination %s: %w", destination, err)
		}
	}

	var published []string
	rollback := func() error {
		var rollbackErr error
		for _, path := range published {
			rollbackErr = errors.Join(rollbackErr, os.RemoveAll(path))
		}
		return rollbackErr
	}
	for _, entry := range stagedEntries {
		sourcePath := filepath.Join(extractedPath, entry.Name())
		destination := filepath.Join(item.Path, entry.Name())
		if err := os.Rename(sourcePath, destination); err != nil {
			return errors.Join(
				fmt.Errorf("publish volume copy-up entry %s: %w", entry.Name(), err),
				rollback(),
			)
		}
		published = append(published, destination)
	}
	directory, err := os.Open(item.Path)
	if err != nil {
		return fmt.Errorf("open copied-up volume for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	return nil
}

func resolveCopyUpSource(rootPath, mountTarget string) (string, bool, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return "", false, fmt.Errorf("copy-up rootfs must be a clean absolute path")
	}
	rootInfo, err := os.Lstat(rootPath)
	if err != nil {
		return "", false, fmt.Errorf("inspect copy-up rootfs: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", false, fmt.Errorf("copy-up rootfs must be a real directory")
	}
	if !filepath.IsAbs(mountTarget) || filepath.Clean(mountTarget) != mountTarget || mountTarget == string(filepath.Separator) {
		return "", false, fmt.Errorf("copy-up mount target must be a clean absolute non-root path")
	}
	for _, component := range strings.Split(filepath.ToSlash(mountTarget), "/") {
		if component == ".." {
			return "", false, fmt.Errorf("copy-up mount target cannot contain '..'")
		}
	}

	current := rootPath
	relative := strings.TrimPrefix(mountTarget, string(filepath.Separator))
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return "", false, fmt.Errorf("copy-up mount target contains an invalid path component")
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("inspect image mount target %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("image mount target contains symlink component %s", current)
		}
		if !info.IsDir() {
			return "", false, fmt.Errorf("image mount target %s is not a directory", current)
		}
	}
	return current, true, nil
}

func Get(name string) (Volume, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := validateName(name); err != nil {
		return Volume{}, err
	}
	volumes, metadata, err := dirs()
	if err != nil {
		return Volume{}, err
	}
	volume, err := loadMetadata(filepath.Join(metadata, name+".json"))
	if err != nil {
		return Volume{}, err
	}
	path := filepath.Join(volumes, name)
	info, err := os.Lstat(path)
	if err != nil {
		return Volume{}, fmt.Errorf("inspect volume data: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Volume{}, fmt.Errorf("volume data path is not a real directory")
	}
	volume.Path = path
	return volume, nil
}

func List() ([]Volume, error) {
	mu.Lock()
	defer mu.Unlock()
	_, metadata, err := dirs()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(metadata)
	if err != nil {
		return nil, err
	}
	items := make([]Volume, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		volume, err := GetUnlocked(name)
		if err != nil {
			return nil, fmt.Errorf("load volume %s: %w", name, err)
		}
		items = append(items, volume)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// GetUnlocked is used by List while it already owns the package mutex.
func GetUnlocked(name string) (Volume, error) {
	if err := validateName(name); err != nil {
		return Volume{}, err
	}
	volumes, metadata, err := dirs()
	if err != nil {
		return Volume{}, err
	}
	volume, err := loadMetadata(filepath.Join(metadata, name+".json"))
	if err != nil {
		return Volume{}, err
	}
	path := filepath.Join(volumes, name)
	info, err := os.Lstat(path)
	if err != nil {
		return Volume{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Volume{}, fmt.Errorf("volume data path is not a real directory")
	}
	volume.Path = path
	return volume, nil
}

// Measure walks a volume without following symlinks. The result is a
// point-in-time logical-size estimate; files may change during the walk when
// a running container is using the volume.
func Measure(name string) (Usage, error) {
	mu.Lock()
	defer mu.Unlock()
	var usage Usage
	item, err := GetUnlocked(name)
	if err != nil {
		return usage, err
	}
	usage.Name = name
	usage.Snapshot = true
	err = filepath.WalkDir(item.Path, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == item.Path {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			usage.Directories++
		case info.Mode()&os.ModeSymlink != 0:
			usage.Symlinks++
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			usage.SymlinkBytes += int64(len(target))
		case info.Mode().IsRegular():
			usage.Files++
			usage.FileBytes += info.Size()
		default:
			usage.OtherEntries++
		}
		return nil
	})
	if err != nil {
		return Usage{}, fmt.Errorf("measure volume %s: %w", name, err)
	}
	usage.TotalBytes = usage.FileBytes + usage.SymlinkBytes
	return usage, nil
}

// Remove deletes volume data and metadata. Callers must check whether active
// containers reference the path before invoking it.
func Remove(name string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := validateName(name); err != nil {
		return err
	}
	volumes, metadata, err := dirs()
	if err != nil {
		return err
	}
	path := filepath.Join(volumes, name)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect volume %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refusing to remove volume path that is not a real directory")
	}
	metaPath := filepath.Join(metadata, name+".json")
	if _, err := loadMetadata(metaPath); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove volume data: %w", err)
	}
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove volume metadata: %w", err)
	}
	return nil
}

func saveMetadata(path string, volume Volume) error {
	data, err := json.MarshalIndent(volume, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".volume-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(temp)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func loadMetadata(path string) (Volume, error) {
	var volume Volume
	info, err := os.Lstat(path)
	if err != nil {
		return volume, fmt.Errorf("volume does not exist: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxMetadataBytes {
		return volume, fmt.Errorf("volume metadata is not a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return volume, err
	}
	if err := json.Unmarshal(data, &volume); err != nil {
		return volume, fmt.Errorf("decode volume metadata: %w", err)
	}
	if err := validateName(volume.Name); err != nil {
		return Volume{}, fmt.Errorf("invalid name in volume metadata: %w", err)
	}
	return volume, nil
}

// ValidateName checks a volume name without touching the filesystem.
func ValidateName(name string) error { return validateName(name) }

func validateName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("volume name must be between 1 and 64 characters")
	}
	for index, char := range name {
		valid := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || index > 0 && (char == '-' || char == '_' || char == '.')
		if !valid {
			return fmt.Errorf("volume name %q contains an invalid character", name)
		}
	}
	return nil
}

func validateLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return fmt.Errorf("at most 64 volume labels are supported")
	}
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 1024 || strings.ContainsAny(key+value, "\x00\r\n") || strings.Contains(key, "=") {
			return fmt.Errorf("invalid volume label %q", key)
		}
	}
	return nil
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	copyOf := make(map[string]string, len(labels))
	for key, value := range labels {
		copyOf[key] = value
	}
	return copyOf
}
