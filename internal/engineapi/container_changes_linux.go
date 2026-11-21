//go:build linux

package engineapi

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/manit6752025/boxr/internal/state"
	"golang.org/x/sys/unix"
)

const (
	maxDockerDiffEntries = 100000
	maxDockerDiffDepth   = 256
	maxDockerDiffPath    = 4096
)

type dockerFilesystemChange struct {
	Path string
	Kind int
}

func (s *Server) handleContainerChanges(writer http.ResponseWriter, request *http.Request, rawID string) {
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("container changes does not accept query parameters"))
		return
	}
	id, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	record, err := state.Get(id)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	rootFS, err := state.ManagedRootFSDir(record.ID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	overlayDir, err := state.ManagedOverlayDir(record.ID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	if filepath.Clean(record.Options.RootFS) != filepath.Clean(rootFS) ||
		filepath.Clean(record.Options.OverlayDir) != filepath.Clean(overlayDir) {
		writeError(writer, http.StatusConflict, fmt.Errorf("filesystem changes are available only for containers with Boxr-managed image and writable layers"))
		return
	}
	changes, err := scanDockerOverlayChanges(rootFS, filepath.Join(overlayDir, "upper"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container filesystem changes: %w", err))
		return
	}
	writeJSON(writer, http.StatusOK, changes)
}

// scanDockerOverlayChanges converts OverlayFS upper-layer entries into the
// Docker Engine filesystem-change kinds: 0 changed, 1 added, and 2 deleted.
func scanDockerOverlayChanges(lower, upper string) ([]dockerFilesystemChange, error) {
	lowerInfo, err := os.Lstat(lower)
	if err != nil {
		return nil, fmt.Errorf("inspect image rootfs: %w", err)
	}
	if lowerInfo.Mode()&os.ModeSymlink != 0 || !lowerInfo.IsDir() {
		return nil, fmt.Errorf("image rootfs must be a real directory")
	}
	upperInfo, err := os.Lstat(upper)
	if os.IsNotExist(err) {
		return []dockerFilesystemChange{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect OverlayFS upper layer: %w", err)
	}
	if upperInfo.Mode()&os.ModeSymlink != 0 || !upperInfo.IsDir() {
		return nil, fmt.Errorf("OverlayFS upper layer must be a real directory")
	}

	changes := make(map[string]int)
	visited := 0
	if err := walkDockerOverlayChanges(lower, upper, "", changes, &visited, 0); err != nil {
		return nil, err
	}
	result := make([]dockerFilesystemChange, 0, len(changes))
	for path, kind := range changes {
		result = append(result, dockerFilesystemChange{Path: path, Kind: kind})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func walkDockerOverlayChanges(lower, upper, relative string, changes map[string]int, visited *int, depth int) error {
	if depth > maxDockerDiffDepth {
		return fmt.Errorf("container filesystem exceeds maximum diff depth %d", maxDockerDiffDepth)
	}
	upperEntries, err := readOverlayDirectory(upper)
	if err != nil {
		return err
	}
	lowerEntries, err := readOverlayDirectory(lower)
	if err != nil {
		return err
	}

	upperNames := make(map[string]struct{}, len(upperEntries))
	for _, entry := range upperEntries {
		upperNames[entry.Name()] = struct{}{}
	}
	whiteouts := make(map[string]struct{})
	opaque := overlayDirectoryOpaque(upper)
	for _, entry := range upperEntries {
		name := entry.Name()
		upperPath := filepath.Join(upper, name)
		if name == ".wh..wh..opq" && isOverlayOpaqueMarker(upperPath, entry) {
			opaque = true
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("inspect upper-layer entry %q: %w", name, infoErr)
		}
		whiteoutName, whiteout := overlayWhiteout(upperPath, entry, info)
		if whiteout {
			lowerPath := filepath.Join(lower, whiteoutName)
			if _, statErr := os.Lstat(lowerPath); statErr == nil {
				path := joinDockerDiffPath(relative, whiteoutName)
				if err := addDockerDiffChange(changes, visited, path, 2); err != nil {
					return err
				}
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("inspect whiteouted lower path %q: %w", whiteoutName, statErr)
			}
			whiteouts[whiteoutName] = struct{}{}
			continue
		}

		lowerPath := filepath.Join(lower, name)
		lowerInfo, statErr := os.Lstat(lowerPath)
		lowerExists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect lower-layer path %q: %w", name, statErr)
		}
		kind := 1
		if lowerExists {
			kind = 0
		}
		path := joinDockerDiffPath(relative, name)
		if err := addDockerDiffChange(changes, visited, path, kind); err != nil {
			return err
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			childLower := lowerPath
			if !lowerExists || !lowerInfo.IsDir() || lowerInfo.Mode()&os.ModeSymlink != 0 {
				childLower = ""
			}
			if err := walkDockerOverlayChanges(childLower, upperPath, path, changes, visited, depth+1); err != nil {
				return err
			}
		}
	}

	if opaque {
		for _, entry := range lowerEntries {
			name := entry.Name()
			if _, present := upperNames[name]; present {
				continue
			}
			if _, whiteouted := whiteouts[name]; whiteouted {
				continue
			}
			lowerPath := filepath.Join(lower, name)
			path := joinDockerDiffPath(relative, name)
			if err := appendDeletedDockerTree(lowerPath, path, changes, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

func readOverlayDirectory(path string) ([]fs.DirEntry, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read OverlayFS comparison directory %q: %w", path, err)
	}
	return entries, nil
}

func overlayWhiteout(path string, entry fs.DirEntry, info fs.FileInfo) (string, bool) {
	name := entry.Name()
	if strings.HasPrefix(name, ".wh.") && name != ".wh..wh..opq" &&
		info.Mode().IsRegular() && info.Size() == 0 {
		target := strings.TrimPrefix(name, ".wh.")
		if target != "" && target != "." && target != ".." {
			return target, true
		}
	}
	if info.Mode().IsRegular() && hasOverlayXattr(path, "trusted.overlay.whiteout") {
		return name, true
	}
	if info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice != 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Rdev == 0 {
			return name, true
		}
	}
	return "", false
}

func isOverlayOpaqueMarker(path string, entry fs.DirEntry) bool {
	info, err := entry.Info()
	return err == nil && info.Mode().IsRegular() && info.Size() == 0
}

func overlayDirectoryOpaque(path string) bool {
	return path != "" && (hasOverlayXattr(path, "trusted.overlay.opaque") ||
		hasOverlayXattr(path, "user.overlay.opaque"))
}

func hasOverlayXattr(path, name string) bool {
	buffer := make([]byte, 32)
	size, err := unix.Getxattr(path, name, buffer)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(buffer[:size])) == "y" || size == 0
}

func joinDockerDiffPath(parent, name string) string {
	joined := filepath.ToSlash(filepath.Join(parent, name))
	return "/" + strings.TrimPrefix(joined, "/")
}

func addDockerDiffChange(changes map[string]int, visited *int, path string, kind int) error {
	if len(path) > maxDockerDiffPath {
		return fmt.Errorf("container filesystem path exceeds %d bytes", maxDockerDiffPath)
	}
	if _, exists := changes[path]; exists {
		return nil
	}
	(*visited)++
	if *visited > maxDockerDiffEntries {
		return fmt.Errorf("container filesystem diff exceeds %d entries", maxDockerDiffEntries)
	}
	changes[path] = kind
	return nil
}

func appendDeletedDockerTree(root, relative string, changes map[string]int, visited *int) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "." && strings.Count(filepath.ToSlash(rel), "/") >= maxDockerDiffDepth {
			return fmt.Errorf("deleted container filesystem subtree exceeds maximum depth %d", maxDockerDiffDepth)
		}
		name := relative
		if rel != "." {
			name = joinDockerDiffPath(relative, rel)
		}
		return addDockerDiffChange(changes, visited, name, 2)
	})
}
