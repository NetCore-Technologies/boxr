// Package rootfs builds, inspects, and archives Boxr root filesystems.
package rootfs

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultMaxFiles int64 = 100000
	DefaultMaxBytes int64 = 8 << 30
)

type Limits struct {
	MaxFiles int64
	MaxBytes int64
}

type Manifest struct {
	Files       int64  `json:"files"`
	Directories int64  `json:"directories"`
	Symlinks    int64  `json:"symlinks"`
	Bytes       int64  `json:"bytes"`
	OSName      string `json:"os_name,omitempty"`
	OSVersion   string `json:"os_version,omitempty"`
}

// ExtractTarGz safely extracts a gzip-compressed tar archive to a new path.
// It rejects path traversal, escaping symlinks, special files, duplicate
// paths, oversized archives, and archives that attempt to populate a non-empty
// destination. Extraction is staged in a sibling temporary directory and
// atomically renamed into place only after every entry has been validated.
func ExtractTarGz(archivePath, destination string, limits Limits) (Manifest, error) {
	return extractTarArchive(archivePath, destination, limits, true)
}

// ExtractTar safely extracts a plain or gzip-compressed tar archive to a new
// path. It uses the same traversal, link, special-file and size limits as the
// gzip-only rootfs restore operation.
func ExtractTar(archivePath, destination string, limits Limits) (Manifest, error) {
	return extractTarArchive(archivePath, destination, limits, false)
}

func extractTarArchive(archivePath, destination string, limits Limits, requireGzip bool) (Manifest, error) {
	var manifest Manifest
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = DefaultMaxFiles
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = DefaultMaxBytes
	}
	archivePath, err := filepath.Abs(archivePath)
	if err != nil {
		return manifest, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return manifest, err
	}
	if destination == string(filepath.Separator) {
		return manifest, fmt.Errorf("refusing to replace filesystem root")
	}
	if _, err := os.Lstat(destination); err == nil {
		return manifest, fmt.Errorf("destination %s already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return manifest, err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return manifest, fmt.Errorf("open rootfs archive: %w", err)
	}
	defer archive.Close()
	buffered := bufio.NewReader(archive)
	magic, peekErr := buffered.Peek(2)
	isGzip := peekErr == nil && magic[0] == 0x1f && magic[1] == 0x8b
	if requireGzip && !isGzip {
		return manifest, fmt.Errorf("archive is not gzip-compressed")
	}
	var source io.Reader = buffered
	if isGzip {
		compressed, gzipErr := gzip.NewReader(buffered)
		if gzipErr != nil {
			return manifest, fmt.Errorf("read gzip header: %w", gzipErr)
		}
		defer compressed.Close()
		source = compressed
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return manifest, fmt.Errorf("create destination parent: %w", err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".boxr-rootfs-*")
	if err != nil {
		return manifest, fmt.Errorf("create staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	stagingRoot, err := os.OpenRoot(staging)
	if err != nil {
		return manifest, fmt.Errorf("open extraction root: %w", err)
	}
	defer stagingRoot.Close()
	seen := make(map[string]bool)
	tarReader := tar.NewReader(source)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return manifest, fmt.Errorf("read tar entry: %w", nextErr)
		}
		if header.Size < 0 {
			return manifest, fmt.Errorf("entry %q has a negative size", header.Name)
		}
		if manifest.Files+manifest.Directories+manifest.Symlinks >= limits.MaxFiles {
			return manifest, fmt.Errorf("archive exceeds the %d-entry limit", limits.MaxFiles)
		}
		if header.Name == "." || header.Name == "./" {
			continue
		}
		name, err := cleanArchiveName(header.Name)
		if err != nil {
			return manifest, fmt.Errorf("invalid archive path %q: %w", header.Name, err)
		}
		if seen[name] {
			return manifest, fmt.Errorf("archive contains duplicate path %q", name)
		}
		seen[name] = true
		target, err := safeArchiveTarget(stagingRoot, name)
		if err != nil {
			return manifest, fmt.Errorf("unsafe archive path %q: %w", name, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := stagingRoot.MkdirAll(target, safeMode(header.Mode, true)); err != nil {
				return manifest, fmt.Errorf("create directory %q: %w", name, err)
			}
			if err := stagingRoot.Chmod(target, safeMode(header.Mode, true)); err != nil {
				return manifest, fmt.Errorf("set directory mode %q: %w", name, err)
			}
			manifest.Directories++
		case tar.TypeReg, tar.TypeRegA:
			if header.Size > limits.MaxBytes-manifest.Bytes {
				return manifest, fmt.Errorf("archive exceeds the %d-byte uncompressed limit", limits.MaxBytes)
			}
			file, err := stagingRoot.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, safeMode(header.Mode, false))
			if err != nil {
				return manifest, fmt.Errorf("create file %q: %w", name, err)
			}
			written, copyErr := io.CopyN(file, tarReader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return manifest, fmt.Errorf("extract file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return manifest, fmt.Errorf("close file %q: %w", name, closeErr)
			}
			if written != header.Size {
				return manifest, fmt.Errorf("file %q was truncated", name)
			}
			manifest.Files++
			manifest.Bytes += written
		case tar.TypeSymlink:
			linkTarget, err := safeSymlinkTarget(name, header.Linkname)
			if err != nil {
				return manifest, fmt.Errorf("unsafe symlink %q: %w", name, err)
			}
			if err := stagingRoot.Symlink(linkTarget, target); err != nil {
				return manifest, fmt.Errorf("create symlink %q: %w", name, err)
			}
			manifest.Symlinks++
		case tar.TypeLink:
			linkName, err := cleanArchiveName(header.Linkname)
			if err != nil {
				return manifest, fmt.Errorf("unsafe hard link target %q: %w", header.Linkname, err)
			}
			linkTarget, err := safeArchiveTarget(stagingRoot, linkName)
			if err != nil {
				return manifest, fmt.Errorf("unsafe hard link target %q: %w", header.Linkname, err)
			}
			info, err := stagingRoot.Lstat(linkTarget)
			if err != nil || !info.Mode().IsRegular() {
				return manifest, fmt.Errorf("hard link target %q must be an existing regular file", linkName)
			}
			if err := stagingRoot.Link(linkTarget, target); err != nil {
				return manifest, fmt.Errorf("create hard link %q: %w", name, err)
			}
			manifest.Files++
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			// The tar reader normally consumes PAX metadata before returning an entry.
			continue
		default:
			return manifest, fmt.Errorf("unsupported special file type %d at %q", header.Typeflag, name)
		}
	}
	if manifest.Files+manifest.Directories+manifest.Symlinks == 0 {
		return manifest, fmt.Errorf("archive is empty")
	}
	if err := stagingRoot.Close(); err != nil {
		return manifest, fmt.Errorf("close extraction root: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		return manifest, fmt.Errorf("publish extracted rootfs: %w", err)
	}
	committed = true
	return manifest, nil
}

// PackTarGz writes a deterministic gzip tar archive. Symlinks are archived as
// links (never followed); paths are relative to the rootfs directory.
func PackTarGz(source, destination string) (Manifest, error) {
	var manifest Manifest
	source, err := filepath.Abs(source)
	if err != nil {
		return manifest, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return manifest, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return manifest, fmt.Errorf("inspect rootfs: %w", err)
	}
	if !info.IsDir() {
		return manifest, fmt.Errorf("rootfs source must be a directory")
	}
	rel, err := filepath.Rel(source, destination)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return manifest, fmt.Errorf("archive destination must be outside the rootfs")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return manifest, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".boxr-rootfs-*.tar.gz")
	if err != nil {
		return manifest, err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	compressed := gzip.NewWriter(temporary)
	compressed.Header.ModTime = time.Unix(0, 0)
	writer := tar.NewWriter(compressed)
	walkErr := filepath.Walk(source, func(filePath string, fileInfo os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == source {
			return nil
		}
		relative, err := filepath.Rel(source, filePath)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(fileInfo, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		// Normalize host metadata so equivalent rootfs trees produce
		// byte-identical archives regardless of build time or file owner.
		header.ModTime = time.Unix(0, 0).UTC()
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.PAXRecords = nil
		if fileInfo.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(filePath)
			if err != nil {
				return err
			}
			header.Linkname = link
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if fileInfo.Mode().IsRegular() {
			file, err := os.Open(filePath)
			if err != nil {
				return err
			}
			written, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			manifest.Files++
			manifest.Bytes += written
		} else if fileInfo.IsDir() {
			manifest.Directories++
		} else if fileInfo.Mode()&os.ModeSymlink != 0 {
			manifest.Symlinks++
		} else {
			return fmt.Errorf("cannot archive special file %s", filePath)
		}
		return nil
	})
	if walkErr != nil {
		_ = writer.Close()
		_ = compressed.Close()
		_ = temporary.Close()
		return manifest, fmt.Errorf("walk rootfs: %w", walkErr)
	}
	if err := writer.Close(); err != nil {
		_ = compressed.Close()
		_ = temporary.Close()
		return manifest, err
	}
	if err := compressed.Close(); err != nil {
		_ = temporary.Close()
		return manifest, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return manifest, err
	}
	if err := temporary.Close(); err != nil {
		return manifest, err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return manifest, fmt.Errorf("publish rootfs archive: %w", err)
	}
	committed = true
	return manifest, nil
}

func Inspect(root string) (Manifest, error) {
	var manifest Manifest
	root, err := filepath.Abs(root)
	if err != nil {
		return manifest, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return manifest, err
	}
	if !info.IsDir() {
		return manifest, fmt.Errorf("rootfs must be a directory")
	}
	err = filepath.Walk(root, func(filePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			manifest.Symlinks++
		case info.IsDir():
			manifest.Directories++
		case info.Mode().IsRegular():
			manifest.Files++
			manifest.Bytes += info.Size()
		default:
			return fmt.Errorf("special file is present: %s", filePath)
		}
		return nil
	})
	if err != nil {
		return manifest, err
	}
	if data, err := os.ReadFile(filepath.Join(root, "etc", "os-release")); err == nil {
		values := parseOSRelease(string(data))
		manifest.OSName = values["PRETTY_NAME"]
		if manifest.OSName == "" {
			manifest.OSName = values["NAME"]
		}
		manifest.OSVersion = values["VERSION_ID"]
	}
	return manifest, nil
}

func cleanArchiveName(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute, empty or invalid path")
	}
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes archive root")
	}
	for _, component := range strings.Split(clean, "/") {
		if component == "" || component == "." || component == ".." || strings.Contains(component, ":") {
			return "", fmt.Errorf("invalid path component")
		}
	}
	return clean, nil
}

// safeArchiveTarget validates and creates only real directory parents beneath
// the extraction root. os.Root keeps every subsequent operation confined to
// that directory, including when an archive contains symlink entries.
func safeArchiveTarget(root *os.Root, name string) (string, error) {
	clean, err := cleanArchiveName(name)
	if err != nil {
		return "", err
	}
	parts := strings.Split(clean, "/")
	for index := 1; index < len(parts); index++ {
		parent := strings.Join(parts[:index], "/")
		info, statErr := root.Lstat(parent)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			if err := root.Mkdir(parent, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
		case statErr != nil:
			return "", statErr
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			return "", fmt.Errorf("archive parent %q is not a real directory", parent)
		}
	}
	return filepath.FromSlash(clean), nil
}

func safeSymlinkTarget(name, linkName string) (string, error) {
	if linkName == "" || strings.ContainsRune(linkName, '\x00') {
		return "", fmt.Errorf("empty or NUL link target")
	}
	if filepath.IsAbs(linkName) || strings.HasPrefix(linkName, "/") {
		// Absolute symlinks in a container refer to the container root. Convert
		// them to relative links so extraction never follows a host absolute path.
		target := strings.TrimLeft(filepath.ToSlash(linkName), "/")
		base := path.Dir(name)
		if base == "." {
			base = ""
		}
		if base == "" {
			return target, nil
		}
		return filepath.ToSlash(relativePath(base, target)), nil
	}
	resolved := path.Clean(path.Join(path.Dir(name), linkName))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("link target escapes rootfs")
	}
	return linkName, nil
}

func relativePath(from, to string) string {
	fromParts := strings.Split(from, "/")
	toParts := strings.Split(to, "/")
	for len(fromParts) > 0 && len(toParts) > 0 && fromParts[0] == toParts[0] {
		fromParts, toParts = fromParts[1:], toParts[1:]
	}
	parts := make([]string, 0, len(fromParts)+len(toParts))
	for range fromParts {
		parts = append(parts, "..")
	}
	parts = append(parts, toParts...)
	return strings.Join(parts, "/")
}

func safeMode(mode int64, directory bool) os.FileMode {
	permissions := os.FileMode(mode) & 0777 // deliberately strip setuid/setgid/sticky bits from archive input
	if directory && permissions == 0 {
		return 0755
	}
	if !directory && permissions == 0 {
		return 0644
	}
	return permissions
}

func parseOSRelease(contents string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, "\"'")
		values[strings.TrimSpace(key)] = value
	}
	return values
}
