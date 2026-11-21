package engineapi

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

const (
	// Keep the HTTP upload bound aligned with the Docker archive decoder's
	// uncompressed archive limit. The decoder separately bounds gzip/zstd
	// expansion and each layer.
	maxDockerArchiveRequestBytes = int64(16 << 30)
	maxDockerArchiveImageCount   = 128
)

func imageArchiveErrorStatus(err error) int {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not found"):
		return http.StatusNotFound
	case strings.Contains(message, "invalid"),
		strings.Contains(message, "unsupported"),
		strings.Contains(message, "duplicate"),
		strings.Contains(message, "must contain"),
		strings.Contains(message, "exceeds"),
		strings.Contains(message, "between"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleImageExportMany(writer http.ResponseWriter, request *http.Request) {
	names := request.URL.Query()["names"]
	s.handleImageExportReferences(writer, request, names)
}

func (s *Server) handleImageExport(writer http.ResponseWriter, request *http.Request, rawReference string) {
	reference, err := decodeDockerImageReference(rawReference)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	s.handleImageExportReferences(writer, request, []string{reference})
}

func (s *Server) handleImageExportReferences(writer http.ResponseWriter, request *http.Request, references []string) {
	if len(references) == 0 || len(references) > maxDockerArchiveImageCount {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("Docker image export requires between 1 and %d names", maxDockerArchiveImageCount))
		return
	}
	for _, reference := range references {
		if strings.TrimSpace(reference) == "" {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("Docker image export names must not be empty"))
			return
		}
	}

	directory, err := os.MkdirTemp("", "boxr-engine-image-export-*")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create Docker image export workspace: %w", err))
		return
	}
	defer os.RemoveAll(directory)
	archivePath := filepath.Join(directory, "images.tar")

	s.imageMu.Lock()
	err = imageStore.SaveDockerMany(references, archivePath)
	s.imageMu.Unlock()
	if err != nil {
		writeError(writer, imageArchiveErrorStatus(err), err)
		return
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("open Docker image export: %w", err))
		return
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect Docker image export: %w", err))
		return
	}
	writer.Header().Set("Content-Type", "application/x-tar")
	writer.Header().Set("Content-Disposition", `attachment; filename="boxr-images.tar"`)
	http.ServeContent(writer, request, "boxr-images.tar", info.ModTime(), archive)
}

func (s *Server) handleImageLoad(writer http.ResponseWriter, request *http.Request) {
	quietValue, err := singleQueryValue(request.URL.Query(), "quiet")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	quiet := false
	if quietValue != "" {
		quiet, err = parseBooleanFilter(quietValue)
		if err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid quiet parameter: %w", err))
			return
		}
	}

	directory, err := os.MkdirTemp("", "boxr-engine-image-load-*")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create Docker image import workspace: %w", err))
		return
	}
	defer os.RemoveAll(directory)
	archivePath := filepath.Join(directory, "images.tar")
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create Docker image import staging file: %w", err))
		return
	}
	written, copyErr := io.Copy(archive, io.LimitReader(request.Body, maxDockerArchiveRequestBytes+1))
	if copyErr == nil && written == 0 {
		copyErr = fmt.Errorf("Docker image archive body must not be empty")
	}
	if copyErr == nil && written > maxDockerArchiveRequestBytes {
		copyErr = fmt.Errorf("Docker image archive exceeds the %d-byte upload limit", maxDockerArchiveRequestBytes)
	}
	if copyErr == nil {
		copyErr = archive.Sync()
	}
	closeErr := archive.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		status := http.StatusBadRequest
		if written > maxDockerArchiveRequestBytes {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(writer, status, fmt.Errorf("stage Docker image archive: %w", copyErr))
		return
	}

	// Parse tags before importing. A Docker-save archive can overwrite a tag;
	// never let that retarget an image reference persisted by a container.
	tags, err := imageStore.DockerArchiveTags(archivePath)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect image users before import: %w", err))
		return
	}
	for _, record := range records {
		usedReference := record.Labels["io.boxr.image"]
		for _, tag := range tags {
			if usedReference == tag {
				writeError(writer, http.StatusConflict, fmt.Errorf("image tag %q is referenced by container %s; refusing to replace it", tag, record.ID))
				return
			}
		}
	}

	imported, err := imageStore.LoadDockerArchive(archivePath)
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("load Docker image archive: %w", err))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	if quiet {
		return
	}
	for _, item := range imported {
		_, _ = fmt.Fprintf(writer, "{\"stream\":%q}\n", "Loaded image: "+item.Reference+"\n")
	}
}
