//go:build linux

package engineapi

import (
	"archive/tar"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

func (s *Server) handleContainerExport(writer http.ResponseWriter, request *http.Request, rawID string) {
	id, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("container export does not accept query parameters"))
		return
	}
	unlock := s.containerTransitions.lock(id)
	defer unlock()
	record, err := state.Get(id)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Labels["io.boxr.image"] == "" {
		writeError(writer, http.StatusConflict, fmt.Errorf("export is available only for image-backed Engine API containers"))
		return
	}
	baseImage, err := imageStore.Inspect(record.Labels["io.boxr.image"])
	if err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("inspect container base image: %w", err))
		return
	}
	excluded, err := dockerCommitExcludedMounts(record, baseImage.RuntimeConfig)
	if err != nil {
		writeError(writer, http.StatusConflict, err)
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
		writeError(writer, http.StatusConflict, fmt.Errorf("export requires Boxr-managed image and writable layers"))
		return
	}

	live := record.Status == "running" || record.Status == "paused"
	verifyLive := func() error {
		if record.PID <= 1 || !state.ProcessAlive(record.PID) {
			return fmt.Errorf("container process is no longer running")
		}
		if err := state.VerifyProcessIdentity(record); err != nil {
			return fmt.Errorf("container process identity changed: %w", err)
		}
		if s.verifyExecPID == nil {
			return fmt.Errorf("container process verification is unavailable")
		}
		if err := s.verifyExecPID(record); err != nil {
			return fmt.Errorf("Boxr process identity verification failed: %w", err)
		}
		return nil
	}

	upperRoot := ""
	switch record.Status {
	case "running", "paused":
		if err := verifyLive(); err != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("refuse export of an unverified container process: %w", err))
			return
		}
		// Keep the final "/." component: it resolves procfs's magic root
		// symlink to the verified container root directory.
		lowerRoot = "/proc/" + strconv.Itoa(record.PID) + "/root/."
	case "created", "exited", "failed":
		if err := verifyDockerCommitDirectory(lowerRoot); err != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("verify container image rootfs: %w", err))
			return
		}
		if err := verifyDockerCommitOptionalDirectory(overlayDir); err != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("verify container writable layer: %w", err))
			return
		}
		if _, statErr := os.Lstat(overlayDir); statErr == nil {
			candidate := filepath.Join(overlayDir, "upper")
			if err := verifyDockerCommitOptionalDirectory(candidate); err != nil {
				writeError(writer, http.StatusConflict, fmt.Errorf("verify container writable upperdir: %w", err))
				return
			}
			if _, statErr := os.Lstat(candidate); statErr == nil {
				upperRoot = candidate
			} else if !os.IsNotExist(statErr) {
				writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container writable upperdir: %w", statErr))
				return
			}
		} else if !os.IsNotExist(statErr) {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container writable layer: %w", statErr))
			return
		}
	default:
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s cannot be exported from state %q", record.ID, record.Status))
		return
	}

	temporary, err := os.CreateTemp("", ".boxr-container-export-*.tar")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create container export staging file: %w", err))
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("secure container export staging file: %w", err))
		return
	}
	limiter := &dockerArchiveLimitWriter{writer: temporary, remaining: maxDockerCommitBytes}
	tarWriter := tar.NewWriter(limiter)
	visited := 0
	if err := addDockerArchiveDirectoryContentsExcluding(lowerRoot, upperRoot, ".", tarWriter, &visited, 0, limiter, excluded); err != nil {
		_ = tarWriter.Close()
		_ = temporary.Close()
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("export container filesystem: %w", err))
		return
	}
	if err := tarWriter.Close(); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("finish container filesystem export: %w", err))
		return
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("sync container export: %w", err))
		return
	}
	if live {
		if err := verifyLive(); err != nil {
			_ = temporary.Close()
			writeError(writer, http.StatusConflict, fmt.Errorf("container process changed during export: %w", err))
			return
		}
	}
	size, err := temporary.Seek(0, io.SeekEnd)
	if err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("measure container export: %w", err))
		return
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		_ = temporary.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("rewind container export: %w", err))
		return
	}
	writer.Header().Set("Content-Type", "application/x-tar")
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.Header().Set("Content-Disposition", "attachment")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, temporary)
	_ = temporary.Close()
}
