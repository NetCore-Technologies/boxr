//go:build linux

package engineapi

import (
	"archive/tar"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/manit6752025/boxr/internal/rootfs"
	"github.com/manit6752025/boxr/internal/state"
)

const (
	maxDockerArchiveUploadBytes int64 = 1 << 30
	maxDockerArchiveExpanded    int64 = 512 << 20
	maxDockerArchiveEntries           = 100000
	maxDockerArchiveDepth             = 256
	maxDockerArchivePath              = 4096
)

type dockerArchiveChild struct {
	lowerPath string
	upperPath string
}

type dockerArchiveLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *dockerArchiveLimitWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("container archive exceeds the %d-byte response limit", maxDockerArchiveExpanded)
	}
	written, err := w.writer.Write(data)
	w.remaining -= int64(written)
	return written, err
}

func cleanDockerArchivePath(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\x00\r\n\\") {
		return "", fmt.Errorf("path must be non-empty and contain no NUL, newline or backslash")
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return "", fmt.Errorf("container path cannot contain '..'")
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "/" {
		return "", nil
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	if len(cleaned) > maxDockerArchivePath {
		return "", fmt.Errorf("container path exceeds %d bytes", maxDockerArchivePath)
	}
	return cleaned, nil
}

func dockerArchivePathFlags(value string) (relative string, contentsOnly, directoryExpected bool, err error) {
	contentsOnly = strings.HasSuffix(value, "/.")
	directoryExpected = contentsOnly || strings.HasSuffix(value, "/")
	relative, err = cleanDockerArchivePath(value)
	return relative, contentsOnly, directoryExpected, err
}

func dockerArchivePathUsesRestrictedMount(relative string) bool {
	if relative == "" {
		return false
	}
	first, _, _ := strings.Cut(relative, "/")
	switch first {
	case "proc", "sys", "dev":
		return true
	default:
		return false
	}
}

func (s *Server) handleContainerArchive(writer http.ResponseWriter, request *http.Request, rawID string) {
	id, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	query := request.URL.Query()
	if len(query["path"]) != 1 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("exactly one path query parameter is required"))
		return
	}
	for key := range query {
		if key != "path" && key != "noOverwriteDirNonDir" && key != "copyUIDGID" {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("unsupported archive query parameter %q", key))
			return
		}
	}
	containerPath, contentsOnly, directoryExpected, err := dockerArchivePathFlags(query.Get("path"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	noOverwrite := false
	copyUIDGID := false
	if request.Method == http.MethodPut {
		if values := query["noOverwriteDirNonDir"]; len(values) > 1 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("noOverwriteDirNonDir may be supplied once"))
			return
		} else if len(values) == 1 {
			noOverwrite, err = strconv.ParseBool(values[0])
			if err != nil {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid noOverwriteDirNonDir value"))
				return
			}
		}
		if values := query["copyUIDGID"]; len(values) > 1 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("copyUIDGID may be supplied once"))
			return
		} else if len(values) == 1 {
			copyUIDGID, err = strconv.ParseBool(values[0])
			if err != nil {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid copyUIDGID value"))
				return
			}
		}
		if copyUIDGID {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("copyUIDGID is not supported; archive ownership is intentionally not applied"))
			return
		}
		// noOverwriteDirNonDir is enforced against the merged container view
		// once the uploaded archive has been fully validated.
	} else if _, exists := query["noOverwriteDirNonDir"]; exists {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("noOverwriteDirNonDir is only valid for PUT"))
		return
	} else if _, exists := query["copyUIDGID"]; exists {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("copyUIDGID is only valid for PUT"))
		return
	}

	unlock := s.containerTransitions.lock(id)
	defer unlock()
	record, err := state.Get(id)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	lowerRoot, err := state.ManagedRootFSDir(record.ID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	overlayDir, err := state.ManagedOverlayDir(record.ID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	if filepath.Clean(record.Options.RootFS) != filepath.Clean(lowerRoot) ||
		filepath.Clean(record.Options.OverlayDir) != filepath.Clean(overlayDir) {
		writeError(writer, http.StatusConflict, fmt.Errorf("archive copy is available only for containers with Boxr-managed image and writable layers"))
		return
	}
	lowerInfo, err := os.Lstat(lowerRoot)
	if err != nil || lowerInfo.Mode()&os.ModeSymlink != 0 || !lowerInfo.IsDir() {
		writeError(writer, http.StatusConflict, fmt.Errorf("container image rootfs is not a verified Boxr-managed directory"))
		return
	}
	if overlayInfo, overlayErr := os.Lstat(overlayDir); overlayErr == nil {
		if overlayInfo.Mode()&os.ModeSymlink != 0 || !overlayInfo.IsDir() {
			writeError(writer, http.StatusConflict, fmt.Errorf("container writable-layer directory is not a real directory"))
			return
		}
	} else if !errors.Is(overlayErr, os.ErrNotExist) {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container writable-layer directory: %w", overlayErr))
		return
	} else if request.Method == http.MethodPut {
		parentInfo, parentErr := os.Lstat(filepath.Dir(overlayDir))
		if parentErr != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			writeError(writer, http.StatusConflict, fmt.Errorf("container writable-layer parent is not a verified directory"))
			return
		}
		if err := os.Mkdir(overlayDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("create container writable-layer directory: %w", err))
			return
		}
	}
	upperRoot := filepath.Join(overlayDir, "upper")
	if info, statErr := os.Lstat(upperRoot); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			writeError(writer, http.StatusConflict, fmt.Errorf("container writable layer is not a real directory"))
			return
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container writable layer: %w", statErr))
		return
	}

	live := record.Status == "running" || record.Status == "paused"
	if live && dockerArchivePathUsesRestrictedMount(containerPath) {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("copying through /proc, /sys or /dev is not supported"))
		return
	}
	if live {
		if record.PID <= 1 || !state.ProcessAlive(record.PID) {
			writeError(writer, http.StatusConflict, fmt.Errorf("container process is no longer running"))
			return
		}
		if err := state.VerifyProcessIdentity(record); err != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("refuse archive access to an unverified container process: %w", err))
			return
		}
		if s.verifyExecPID == nil {
			writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("container process verification is unavailable"))
			return
		}
		if err := s.verifyExecPID(record); err != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("refuse archive access to an unverified Boxr process: %w", err))
			return
		}
	}

	switch request.Method {
	case http.MethodGet, http.MethodHead:
		readLower, readUpper := lowerRoot, upperRoot
		if live {
			// Resolve live bind mounts and writable files through the verified
			// container root without opening host /proc, /sys or /dev paths.
			readLower = filepath.Join("/proc", strconv.Itoa(record.PID), "root", ".")
			readUpper = ""
		}
		s.handleDockerArchiveGet(writer, request, readLower, readUpper, containerPath, contentsOnly, directoryExpected)
	case http.MethodPut:
		if upperInfo, statErr := os.Lstat(upperRoot); errors.Is(statErr, os.ErrNotExist) {
			if err := os.MkdirAll(upperRoot, 0700); err != nil {
				writeError(writer, http.StatusInternalServerError, fmt.Errorf("create container writable layer: %w", err))
				return
			}
		} else if statErr != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container writable layer: %w", statErr))
			return
		} else if upperInfo.Mode()&os.ModeSymlink != 0 || !upperInfo.IsDir() {
			writeError(writer, http.StatusConflict, fmt.Errorf("container writable layer is not a real directory"))
			return
		}
		writeRoot := upperRoot
		if live {
			// Modify the mounted container view rather than mutating an OverlayFS
			// upperdir behind a live mount.
			writeRoot = filepath.Join("/proc", strconv.Itoa(record.PID), "root", ".")
		}
		s.handleDockerArchivePut(writer, request, writeRoot, writeRoot, containerPath, noOverwrite)
	default:
		writeMethodErrorFor(writer, http.MethodGet+", "+http.MethodHead+", "+http.MethodPut)
	}
}

func (s *Server) handleDockerArchiveGet(writer http.ResponseWriter, request *http.Request, lowerRoot, upperRoot, containerPath string, contentsOnly, directoryExpected bool) {
	lowerPath, upperPath, info, err := resolveDockerOverlayPath(lowerRoot, upperRoot, containerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			writeError(writer, http.StatusNotFound, fmt.Errorf("container path does not exist"))
		} else {
			writeError(writer, http.StatusConflict, fmt.Errorf("resolve container path: %w", err))
		}
		return
	}
	if directoryExpected && !info.IsDir() {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("container path was required to be a directory"))
		return
	}
	linkTarget := ""
	if info.Mode()&os.ModeSymlink != 0 {
		chosen := upperPath
		if chosen == "" {
			chosen = lowerPath
		}
		linkTarget, err = os.Readlink(chosen)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("read container symlink: %w", err))
			return
		}
	}
	statName := path.Base(containerPath)
	if containerPath == "" {
		statName = "/"
	}
	stat := map[string]any{
		"name": statName, "size": info.Size(),
		"mode": int64(info.Mode()), "mtime": info.ModTime().UTC().Format(time.RFC3339Nano), "linkTarget": linkTarget,
	}
	statJSON, err := json.Marshal(stat)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("encode container path metadata: %w", err))
		return
	}
	writer.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(statJSON))
	if request.Method == http.MethodHead {
		writer.Header().Set("Content-Type", "application/x-tar")
		writer.WriteHeader(http.StatusOK)
		return
	}

	temporary, err := os.CreateTemp("", ".boxr-docker-archive-*.tar")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create archive staging file: %w", err))
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("secure archive staging file: %w", err))
		return
	}
	limited := &dockerArchiveLimitWriter{writer: temporary, remaining: maxDockerArchiveExpanded}
	tarWriter := tar.NewWriter(limited)
	tarName := path.Base(containerPath)
	if containerPath == "" || contentsOnly {
		tarName = "."
	}
	visited := 0
	var archiveErr error
	if contentsOnly && info.IsDir() {
		archiveErr = addDockerArchiveDirectoryContents(lowerPath, upperPath, tarName, tarWriter, &visited, 0, limited)
	} else {
		archiveErr = addDockerArchiveNode(lowerPath, upperPath, tarName, tarWriter, &visited, 0, limited)
	}
	if archiveErr != nil {
		err = archiveErr
		_ = tarWriter.Close()
		_ = temporary.Close()
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("archive container path: %w", err))
		return
	}
	if err := tarWriter.Close(); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("finish container archive: %w", err))
		return
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("sync container archive: %w", err))
		return
	}
	size, err := temporary.Seek(0, io.SeekEnd)
	if err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("measure container archive: %w", err))
		return
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("rewind container archive: %w", err))
		return
	}
	writer.Header().Set("Content-Type", "application/x-tar")
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.Header().Set("Content-Disposition", "attachment")
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(writer, temporary)
	}
	_ = temporary.Close()
}

func addDockerArchiveDirectoryContents(lowerPath, upperPath, archiveName string, tarWriter *tar.Writer, visited *int, depth int, limiter *dockerArchiveLimitWriter) error {
	if depth > maxDockerArchiveDepth {
		return fmt.Errorf("container path exceeds maximum archive depth %d", maxDockerArchiveDepth)
	}
	children, err := dockerOverlayChildren(lowerPath, upperPath)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := children[name]
		if err := addDockerArchiveNode(child.lowerPath, child.upperPath, path.Join(archiveName, name), tarWriter, visited, depth+1, limiter); err != nil {
			return err
		}
	}
	return nil
}

func addDockerArchiveNode(lowerPath, upperPath, archiveName string, tarWriter *tar.Writer, visited *int, depth int, limiter *dockerArchiveLimitWriter) error {
	if depth > maxDockerArchiveDepth {
		return fmt.Errorf("container path exceeds maximum archive depth %d", maxDockerArchiveDepth)
	}
	var lowerInfo, upperInfo os.FileInfo
	var err error
	if lowerPath != "" {
		lowerInfo, err = os.Lstat(lowerPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect lower-layer path: %w", err)
		}
	}
	if upperPath != "" {
		upperInfo, err = os.Lstat(upperPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect upper-layer path: %w", err)
		}
	}
	info, source := lowerInfo, lowerPath
	if upperInfo != nil {
		info, source = upperInfo, upperPath
	}
	if info == nil {
		return os.ErrNotExist
	}
	(*visited)++
	if *visited > maxDockerArchiveEntries {
		return fmt.Errorf("container archive exceeds %d entries", maxDockerArchiveEntries)
	}
	if len(archiveName) > maxDockerArchivePath {
		return fmt.Errorf("archive path exceeds %d bytes", maxDockerArchivePath)
	}
	linkTarget := ""
	if info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err = os.Readlink(source)
		if err != nil {
			return fmt.Errorf("read symlink %q: %w", archiveName, err)
		}
	}
	header, err := tar.FileInfoHeader(info, linkTarget)
	if err != nil {
		return fmt.Errorf("create tar header for %q: %w", archiveName, err)
	}
	header.Name = archiveName
	if info.IsDir() && !strings.HasSuffix(header.Name, "/") {
		header.Name += "/"
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		return err
	}
	switch {
	case info.IsDir():
		children, err := dockerOverlayChildren(lowerPath, upperPath)
		if err != nil {
			return fmt.Errorf("list container directory %q: %w", archiveName, err)
		}
		names := make([]string, 0, len(children))
		for name := range children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := children[name]
			if err := addDockerArchiveNode(child.lowerPath, child.upperPath, path.Join(archiveName, name), tarWriter, visited, depth+1, limiter); err != nil {
				return err
			}
		}
	case info.Mode().IsRegular():
		if info.Size() < 0 || info.Size() > limiter.remaining {
			return fmt.Errorf("file %q exceeds the remaining archive size limit", archiveName)
		}
		file, err := os.Open(source)
		if err != nil {
			return fmt.Errorf("open container file %q: %w", archiveName, err)
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
			_ = file.Close()
			return fmt.Errorf("container file %q changed while archiving", archiveName)
		}
		_, copyErr := io.CopyN(tarWriter, file, info.Size())
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("read container file %q: %w", archiveName, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close container file %q: %w", archiveName, closeErr)
		}
	case info.Mode()&os.ModeSymlink != 0:
		// Symlinks are archived as links. They are never followed.
	default:
		return fmt.Errorf("refuse to archive special file %q", archiveName)
	}
	return nil
}

func dockerOverlayChildren(lowerDir, upperDir string) (map[string]dockerArchiveChild, error) {
	children := make(map[string]dockerArchiveChild)
	whiteouts := make(map[string]struct{})
	opaque := upperDir != "" && overlayDirectoryOpaque(upperDir)
	if upperDir != "" {
		entries, err := readOverlayDirectory(upperDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			entryPath := filepath.Join(upperDir, name)
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("inspect upper-layer entry %q: %w", name, err)
			}
			if name == ".wh..wh..opq" && isOverlayOpaqueMarker(entry) {
				opaque = true
				continue
			}
			target, whiteout := overlayWhiteout(entryPath, entry, info)
			if whiteout {
				whiteouts[target] = struct{}{}
				continue
			}
			children[name] = dockerArchiveChild{upperPath: entryPath}
		}
	}
	if lowerDir != "" && !opaque {
		entries, err := readOverlayDirectory(lowerDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if _, deleted := whiteouts[name]; deleted {
				continue
			}
			child := children[name]
			if child.lowerPath == "" {
				child.lowerPath = filepath.Join(lowerDir, name)
				children[name] = child
			}
		}
	}
	return children, nil
}

const maxDockerArchiveSymlinks = 40

func resolveDockerOverlayPath(lowerRoot, upperRoot, relative string) (string, string, os.FileInfo, error) {
	return resolveDockerOverlayPathWithSymlinks(lowerRoot, upperRoot, relative, 0)
}

func resolveDockerOverlayPathWithSymlinks(lowerRoot, upperRoot, relative string, symlinkDepth int) (string, string, os.FileInfo, error) {
	if symlinkDepth > maxDockerArchiveSymlinks {
		return "", "", nil, fmt.Errorf("container path exceeds %d symlink traversals", maxDockerArchiveSymlinks)
	}
	if relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsRune(relative, '\\') || strings.ContainsRune(relative, '\x00') {
		return "", "", nil, fmt.Errorf("invalid resolved container path")
	}
	if len(relative) > maxDockerArchivePath {
		return "", "", nil, fmt.Errorf("resolved container path exceeds %d bytes", maxDockerArchivePath)
	}
	lowerInfo, err := os.Stat(lowerRoot)
	if err != nil || !lowerInfo.IsDir() {
		return "", "", nil, fmt.Errorf("invalid lower-layer root")
	}
	var upperInfo os.FileInfo
	if upperRoot != "" {
		upperInfo, err = os.Lstat(upperRoot)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", nil, fmt.Errorf("inspect upper-layer root: %w", err)
		}
		if upperInfo != nil && (upperInfo.Mode()&os.ModeSymlink != 0 || !upperInfo.IsDir()) {
			return "", "", nil, fmt.Errorf("upper-layer root must be a real directory")
		}
	}
	if relative == "" {
		if upperInfo != nil {
			info, statErr := os.Stat(upperRoot)
			if statErr != nil {
				return "", "", nil, statErr
			}
			return lowerRoot, upperRoot, info, nil
		}
		return lowerRoot, "", lowerInfo, nil
	}

	lowerCurrent, upperCurrent := lowerRoot, upperRoot
	singleRoot := upperRoot != "" && filepath.Clean(lowerRoot) == filepath.Clean(upperRoot)
	parts := strings.Split(relative, "/")
	for index, name := range parts {
		var lowerChild, upperChild string
		var lowerEntryInfo, upperEntryInfo os.FileInfo
		if upperCurrent != "" && !singleRoot {
			whiteouted, err := dockerOverlayWhiteouts(upperCurrent, name)
			if err != nil {
				return "", "", nil, err
			}
			if whiteouted {
				return "", "", nil, os.ErrNotExist
			}
			candidate := filepath.Join(upperCurrent, name)
			if info, statErr := os.Lstat(candidate); statErr == nil {
				upperChild, upperEntryInfo = candidate, info
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return "", "", nil, statErr
			}
		}
		parentOpaque := upperCurrent != "" && !singleRoot && overlayDirectoryOpaque(upperCurrent)
		if lowerCurrent != "" && !parentOpaque {
			candidate := filepath.Join(lowerCurrent, name)
			if info, statErr := os.Lstat(candidate); statErr == nil {
				lowerChild, lowerEntryInfo = candidate, info
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return "", "", nil, statErr
			}
		}
		if upperChild == "" && lowerChild == "" {
			return "", "", nil, os.ErrNotExist
		}
		chosenInfo := upperEntryInfo
		chosenPath := upperChild
		if chosenInfo == nil {
			chosenInfo = lowerEntryInfo
			chosenPath = lowerChild
		}
		if chosenInfo != nil && chosenInfo.Mode()&os.ModeSymlink != 0 {
			if symlinkDepth >= maxDockerArchiveSymlinks {
				return "", "", nil, fmt.Errorf("container path exceeds %d symlink traversals", maxDockerArchiveSymlinks)
			}
			linkTarget, linkErr := os.Readlink(chosenPath)
			if linkErr != nil {
				return "", "", nil, fmt.Errorf("read container symlink %q: %w", name, linkErr)
			}
			if linkTarget == "" || strings.ContainsRune(linkTarget, '\x00') {
				return "", "", nil, fmt.Errorf("container symlink %q has an invalid target", name)
			}
			parent := strings.Join(parts[:index], "/")
			var target string
			if path.IsAbs(linkTarget) {
				target = strings.TrimPrefix(path.Clean(linkTarget), "/")
			} else {
				target = path.Clean(path.Join(parent, linkTarget))
			}
			remaining := strings.Join(parts[index+1:], "/")
			if remaining != "" {
				target = path.Join(target, remaining)
			}
			if target == "." {
				target = ""
			}
			if target == ".." || strings.HasPrefix(target, "../") {
				return "", "", nil, fmt.Errorf("container symlink %q escapes the container root", name)
			}
			return resolveDockerOverlayPathWithSymlinks(lowerRoot, upperRoot, target, symlinkDepth+1)
		}
		if index != len(parts)-1 {
			if chosenInfo == nil || !chosenInfo.IsDir() || chosenInfo.Mode()&os.ModeSymlink != 0 {
				return "", "", nil, syscall.ENOTDIR
			}
			if upperChild != "" && upperEntryInfo.IsDir() {
				upperCurrent = upperChild
			} else {
				upperCurrent = ""
			}
			if lowerChild != "" && lowerEntryInfo.IsDir() && (upperChild == "" || upperEntryInfo.IsDir()) &&
				!(upperChild != "" && overlayDirectoryOpaque(upperChild)) {
				lowerCurrent = lowerChild
			} else {
				lowerCurrent = ""
			}
			continue
		}
		return lowerChild, upperChild, chosenInfo, nil
	}
	return "", "", nil, os.ErrNotExist
}

func dockerOverlayWhiteouts(upperDir, name string) (bool, error) {
	entries, err := readOverlayDirectory(upperDir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return false, err
		}
		target, whiteout := overlayWhiteout(filepath.Join(upperDir, entry.Name()), entry, info)
		if whiteout && target == name {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) handleDockerArchivePut(writer http.ResponseWriter, request *http.Request, lowerRoot, upperRoot, containerPath string, noOverwrite bool) {
	lowerDestination, upperDestination, destinationInfo, err := resolveDockerOverlayPath(lowerRoot, upperRoot, containerPath)
	if err != nil {
		writeError(writer, http.StatusNotFound, fmt.Errorf("container destination does not exist"))
		return
	}
	if !destinationInfo.IsDir() || destinationInfo.Mode()&os.ModeSymlink != 0 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("PUT archive path must identify an existing directory"))
		return
	}
	resolvedDestination := containerPath
	resolvedPath, resolvedRoot := upperDestination, upperRoot
	if resolvedPath == "" {
		resolvedPath, resolvedRoot = lowerDestination, lowerRoot
	}
	if resolvedPath != "" {
		relative, relErr := filepath.Rel(resolvedRoot, resolvedPath)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			writeError(writer, http.StatusConflict, fmt.Errorf("resolved container destination is outside its managed root"))
			return
		}
		resolvedDestination = filepath.ToSlash(relative)
		if resolvedDestination == "." {
			resolvedDestination = ""
		}
	}
	if request.ContentLength > maxDockerArchiveUploadBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, fmt.Errorf("archive upload exceeds %d bytes", maxDockerArchiveUploadBytes))
		return
	}
	temporaryDir, err := os.MkdirTemp("", ".boxr-docker-copy-*")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create archive upload staging directory: %w", err))
		return
	}
	defer os.RemoveAll(temporaryDir)
	if err := os.Chmod(temporaryDir, 0700); err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("secure archive upload staging directory: %w", err))
		return
	}
	archivePath := filepath.Join(temporaryDir, "upload.tar")
	archiveFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create archive upload file: %w", err))
		return
	}
	written, copyErr := io.Copy(archiveFile, io.LimitReader(request.Body, maxDockerArchiveUploadBytes+1))
	syncErr := archiveFile.Sync()
	closeErr := archiveFile.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("receive archive upload: %w", firstArchiveError(copyErr, syncErr, closeErr)))
		return
	}
	if written == 0 || written > maxDockerArchiveUploadBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, fmt.Errorf("archive upload must contain between 1 byte and %d bytes", maxDockerArchiveUploadBytes))
		return
	}
	extractedPath := filepath.Join(temporaryDir, "extracted")
	if _, err := rootfs.ExtractTar(archivePath, extractedPath, rootfs.Limits{
		MaxFiles: maxDockerArchiveEntries, MaxBytes: maxDockerArchiveExpanded,
	}); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("validate container archive: %w", err))
		return
	}
	if err := validateDockerArchiveUploadTree(extractedPath); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if noOverwrite {
		if err := validateDockerArchiveNoOverwrite(extractedPath, lowerRoot, upperRoot, resolvedDestination); err != nil {
			writeError(writer, http.StatusConflict, err)
			return
		}
	}
	if err := mergeDockerArchiveTree(extractedPath, upperRoot, resolvedDestination); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("apply container archive: %w", err))
		return
	}
	writer.WriteHeader(http.StatusOK)
}

func validateDockerArchiveNoOverwrite(sourceRoot, lowerRoot, upperRoot, destination string) error {
	return filepath.WalkDir(sourceRoot, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == sourceRoot {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, current)
		if err != nil {
			return err
		}
		target := path.Join(destination, filepath.ToSlash(relative))
		_, _, existingInfo, err := resolveDockerOverlayPath(lowerRoot, upperRoot, target)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect existing path %q: %w", target, err)
		}
		incomingInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if incomingInfo.IsDir() != existingInfo.IsDir() {
			return fmt.Errorf("refusing to replace a directory with a non-directory or vice versa at %q", target)
		}
		return nil
	})
}

func firstArchiveError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func validateDockerArchiveUploadTree(root string) error {
	visited := 0
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		visited++
		if visited > maxDockerArchiveEntries {
			return fmt.Errorf("archive exceeds %d entries", maxDockerArchiveEntries)
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".wh.") || name == ".wh..wh..opq" {
			return fmt.Errorf("archive path %q uses a reserved OverlayFS whiteout name", name)
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if len(filepath.ToSlash(relative)) > maxDockerArchivePath {
			return fmt.Errorf("archive path exceeds %d bytes", maxDockerArchivePath)
		}
		if strings.Count(filepath.ToSlash(relative), "/") >= maxDockerArchiveDepth {
			return fmt.Errorf("archive path exceeds maximum depth %d", maxDockerArchiveDepth)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("archive contains unsupported special file %q", relative)
		}
		return nil
	})
}

func mergeDockerArchiveTree(sourceRoot, upperRoot, destination string) error {
	upperInfo, err := os.Lstat(upperRoot)
	if err != nil || upperInfo.Mode()&os.ModeSymlink != 0 || !upperInfo.IsDir() {
		return fmt.Errorf("writable layer must be a real directory")
	}
	root, err := os.OpenRoot(upperRoot)
	if err != nil {
		return fmt.Errorf("open writable layer: %w", err)
	}
	defer root.Close()
	destination = filepath.ToSlash(destination)
	if destination != "" {
		if err := ensureDockerArchiveUpperParents(root, destination); err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.FromSlash(destination), 0755); err != nil {
			return fmt.Errorf("create writable-layer destination: %w", err)
		}
	}
	return mergeDockerArchiveDirectory(sourceRoot, root, destination)
}

func mergeDockerArchiveDirectory(sourceDir string, root *os.Root, destination string) error {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(sourceDir, entry.Name())
		destinationPath := path.Join(destination, entry.Name())
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return err
		}
		if err := ensureDockerArchiveUpperParents(root, destinationPath); err != nil {
			return err
		}
		parent := path.Dir(destinationPath)
		if parent == "." {
			parent = ""
		}
		whiteout := path.Join(parent, ".wh."+entry.Name())
		if _, err := root.Lstat(filepath.FromSlash(whiteout)); err == nil {
			if err := root.RemoveAll(filepath.FromSlash(whiteout)); err != nil {
				return fmt.Errorf("remove OverlayFS whiteout for %q: %w", destinationPath, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if info.IsDir() {
			if existing, statErr := root.Lstat(filepath.FromSlash(destinationPath)); statErr == nil && (!existing.IsDir() || existing.Mode()&os.ModeSymlink != 0) {
				if err := root.RemoveAll(filepath.FromSlash(destinationPath)); err != nil {
					return err
				}
			} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			if err := root.MkdirAll(filepath.FromSlash(destinationPath), info.Mode().Perm()&0777); err != nil {
				return fmt.Errorf("create archive directory %q: %w", destinationPath, err)
			}
			if err := mergeDockerArchiveDirectory(sourcePath, root, destinationPath); err != nil {
				return err
			}
			if err := root.Chmod(filepath.FromSlash(destinationPath), info.Mode().Perm()&0777); err != nil {
				return fmt.Errorf("set archive directory mode %q: %w", destinationPath, err)
			}
			continue
		}
		if err := root.RemoveAll(filepath.FromSlash(destinationPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replace writable-layer entry %q: %w", destinationPath, err)
		}
		switch {
		case info.Mode().IsRegular():
			if err := copyDockerArchiveFile(sourcePath, root, destinationPath, info.Mode().Perm()&0777); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(sourcePath)
			if err != nil {
				return err
			}
			if err := root.Symlink(linkTarget, filepath.FromSlash(destinationPath)); err != nil {
				return fmt.Errorf("create archive symlink %q: %w", destinationPath, err)
			}
		default:
			return fmt.Errorf("unsupported archive entry %q", destinationPath)
		}
	}
	return nil
}

func copyDockerArchiveFile(sourcePath string, root *os.Root, destination string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open staged archive file: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("staged archive file changed before copy")
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate temporary file name: %w", err)
	}
	parent := path.Dir(destination)
	if parent == "." {
		parent = ""
	}
	temporary := path.Join(parent, ".boxr-copy-"+hex.EncodeToString(nonce))
	file, err := root.OpenFile(filepath.FromSlash(temporary), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create writable-layer temporary file: %w", err)
	}
	written, copyErr := io.Copy(file, source)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = root.Remove(filepath.FromSlash(temporary))
		return fmt.Errorf("write writable-layer file: %w", firstArchiveError(copyErr, syncErr, closeErr))
	}
	if written != info.Size() {
		_ = root.Remove(filepath.FromSlash(temporary))
		return fmt.Errorf("staged archive file changed during copy")
	}
	if err := root.Rename(filepath.FromSlash(temporary), filepath.FromSlash(destination)); err != nil {
		_ = root.Remove(filepath.FromSlash(temporary))
		return fmt.Errorf("publish writable-layer file: %w", err)
	}
	return nil
}

func ensureDockerArchiveUpperParents(root *os.Root, relative string) error {
	relative = path.Clean(relative)
	if relative == "." || relative == "" {
		return nil
	}
	parts := strings.Split(relative, "/")
	for index := 1; index < len(parts); index++ {
		parent := strings.Join(parts[:index], "/")
		info, err := root.Lstat(filepath.FromSlash(parent))
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := root.Mkdir(filepath.FromSlash(parent), 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create writable-layer parent %q: %w", parent, err)
			}
		case err != nil:
			return err
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			return fmt.Errorf("writable-layer parent %q is not a real directory", parent)
		}
	}
	return nil
}
