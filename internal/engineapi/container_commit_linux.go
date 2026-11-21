//go:build linux

package engineapi

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/rootfs"
	"github.com/manit6752025/boxr/internal/state"
)

const (
	maxDockerCommitBodyBytes int64 = 1 << 20
	maxDockerCommitBytes     int64 = 8 << 30
	maxDockerCommitFiles     int64 = 100000
)

func (s *Server) handleContainerCommit(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	for key := range query {
		switch key {
		case "container", "repo", "tag", "comment", "author", "pause", "changes":
		default:
			writeError(writer, http.StatusBadRequest, fmt.Errorf("unsupported commit query parameter %q", key))
			return
		}
	}
	containerID, err := singleQueryValue(query, "container")
	if err != nil || containerID == "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("commit requires exactly one container query parameter"))
		return
	}
	repoName, err := singleQueryValue(query, "repo")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	tag, err := singleQueryValue(query, "tag")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	comment, err := singleQueryValue(query, "comment")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	author, err := singleQueryValue(query, "author")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	changes, err := singleQueryValue(query, "changes")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if changes != "" {
		writeError(writer, http.StatusNotImplemented, fmt.Errorf("Docker commit changes directives are not supported; use Boxr's Dockerfile build endpoint"))
		return
	}
	if raw, exists := query["pause"]; exists {
		if len(raw) != 1 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("pause may be supplied once"))
			return
		}
		if _, err := parseDockerBool(raw[0]); err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("pause must be a boolean"))
			return
		}
	}

	var override imageStore.ImageRuntimeConfig
	data, err := io.ReadAll(io.LimitReader(request.Body, maxDockerCommitBodyBytes+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("read commit configuration: %w", err))
		return
	}
	if int64(len(data)) > maxDockerCommitBodyBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, fmt.Errorf("commit configuration exceeds %d bytes", maxDockerCommitBodyBytes))
		return
	}
	if len(data) > 0 && strings.TrimSpace(string(data)) != "" {
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&override); err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("decode commit configuration: %w", err))
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("commit configuration must contain one JSON object"))
			return
		}
	}

	unlock := s.containerTransitions.lock(containerID)
	defer unlock()
	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	record, err := state.Get(containerID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	switch record.Status {
	case "running", "paused", "restarting":
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s must be stopped before commit; Boxr will not snapshot a changing writable layer", record.ID))
		return
	case "created", "exited", "failed":
	default:
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s cannot be committed from state %q", record.ID, record.Status))
		return
	}
	if record.Labels["io.boxr.image"] == "" {
		writeError(writer, http.StatusConflict, fmt.Errorf("commit is available only for image-backed Engine API containers"))
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
		writeError(writer, http.StatusConflict, fmt.Errorf("commit requires Boxr-managed image and writable layers"))
		return
	}
	if err := verifyDockerCommitDirectory(lowerRoot); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("verify container image rootfs: %w", err))
		return
	}
	if err := verifyDockerCommitOptionalDirectory(overlayDir); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("verify container writable layer: %w", err))
		return
	}
	upperRoot := ""
	if _, err := os.Lstat(overlayDir); err == nil {
		upperRoot = filepath.Join(overlayDir, "upper")
		if err := verifyDockerCommitOptionalDirectory(upperRoot); err != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("verify container writable upperdir: %w", err))
			return
		}
		if _, err := os.Lstat(upperRoot); errors.Is(err, os.ErrNotExist) {
			upperRoot = ""
		} else if err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container writable upperdir: %w", err))
			return
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container writable layer: %w", err))
		return
	}

	baseImage, err := imageStore.Inspect(record.Labels["io.boxr.image"])
	if err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("inspect container base image: %w", err))
		return
	}
	runtimeConfig := dockerCommitRuntimeConfig(record, baseImage.RuntimeConfig)
	applyDockerCommitOverrides(&runtimeConfig, override)
	if author != "" {
		if runtimeConfig.Labels == nil {
			runtimeConfig.Labels = make(map[string]string)
		}
		runtimeConfig.Labels["org.opencontainers.image.authors"] = author
	}
	if comment != "" {
		if runtimeConfig.Labels == nil {
			runtimeConfig.Labels = make(map[string]string)
		}
		runtimeConfig.Labels["org.opencontainers.image.description"] = comment
	}
	if err := imageStore.ValidateBuildSpec(imageStore.BuildSpec{Version: 1, Base: "scratch", Config: runtimeConfig}); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid committed image configuration: %w", err))
		return
	}

	reference, err := dockerCommitReference(record.ID, repoName, tag)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect image references: %w", err))
		return
	}
	for _, candidate := range records {
		if candidate.Labels["io.boxr.image"] == reference {
			writeError(writer, http.StatusConflict, fmt.Errorf("cannot replace image %q while container %s references it", reference, candidate.ID))
			return
		}
	}
	excluded, err := dockerCommitExcludedMounts(record, runtimeConfig)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}

	workspace, err := os.MkdirTemp("", ".boxr-container-commit-*")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create commit workspace: %w", err))
		return
	}
	defer os.RemoveAll(workspace)
	if err := os.Chmod(workspace, 0700); err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("secure commit workspace: %w", err))
		return
	}
	archivePath := filepath.Join(workspace, "rootfs.tar")
	archiveFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create commit rootfs archive: %w", err))
		return
	}
	limiter := &dockerArchiveLimitWriter{writer: archiveFile, remaining: maxDockerCommitBytes}
	tarWriter := tar.NewWriter(limiter)
	visited := 0
	if err := addDockerArchiveDirectoryContentsExcluding(lowerRoot, upperRoot, ".", tarWriter, &visited, 0, limiter, excluded); err != nil {
		_ = tarWriter.Close()
		_ = archiveFile.Close()
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("snapshot container rootfs: %w", err))
		return
	}
	if err := tarWriter.Close(); err != nil {
		_ = archiveFile.Close()
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("finish container rootfs snapshot: %w", err))
		return
	}
	if err := archiveFile.Sync(); err != nil {
		_ = archiveFile.Close()
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("sync container rootfs snapshot: %w", err))
		return
	}
	if err := archiveFile.Close(); err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("close container rootfs snapshot: %w", err))
		return
	}
	rootPath := filepath.Join(workspace, "rootfs")
	if _, err := rootfs.ExtractTar(archivePath, rootPath, rootfs.Limits{
		MaxFiles: maxDockerCommitFiles, MaxBytes: maxDockerCommitBytes,
	}); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("validate committed rootfs snapshot: %w", err))
		return
	}

	architecture := baseImage.Arch
	if architecture == "" {
		architecture = runtime.GOARCH
	}
	committed, err := imageStore.ImportRootFSWithConfig(rootPath, reference, "linux", architecture, baseImage.Variant, runtimeConfig)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("import committed image: %w", err))
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"Id":       dockerImageID(committed.Digest),
		"Warnings": []string{},
	})
}

func parseDockerBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean")
	}
}

func verifyDockerCommitDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", directory)
	}
	return nil
}

func verifyDockerCommitOptionalDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", directory)
	}
	return nil
}

func dockerCommitReference(containerID, repoName, tag string) (string, error) {
	if tag != "" && repoName == "" {
		return "", fmt.Errorf("tag requires repo")
	}
	if repoName == "" {
		return fmt.Sprintf("boxr-committed/container-%s:commit-%d", containerID, time.Now().UTC().UnixNano()), nil
	}
	if strings.ContainsAny(repoName, "\x00\r\n ") || strings.ContainsAny(tag, "\x00\r\n /") {
		return "", fmt.Errorf("repo or tag contains invalid characters")
	}
	lastSlash := strings.LastIndex(repoName, "/")
	lastColon := strings.LastIndex(repoName, ":")
	if lastColon > lastSlash {
		if tag != "" {
			return "", fmt.Errorf("tag must not be supplied when repo already contains a tag")
		}
		if err := imageStore.ValidateReference(repoName); err != nil {
			return "", err
		}
		return repoName, nil
	}
	if tag == "" {
		tag = "latest"
	}
	reference := repoName + ":" + tag
	if err := imageStore.ValidateReference(reference); err != nil {
		return "", err
	}
	return reference, nil
}

func dockerCommitRuntimeConfig(record state.Record, base imageStore.ImageRuntimeConfig) imageStore.ImageRuntimeConfig {
	result := base
	result.Env = append([]string(nil), base.Env...)
	result.Entrypoint = append([]string(nil), base.Entrypoint...)
	result.Cmd = append([]string(nil), base.Cmd...)
	result.Labels = make(map[string]string, len(base.Labels)+len(record.Labels))
	for key, value := range base.Labels {
		result.Labels[key] = value
	}
	for key, value := range record.Labels {
		if !strings.HasPrefix(key, "io.boxr.") {
			result.Labels[key] = value
		}
	}
	if len(record.Options.Env) > 0 {
		result.Env = append([]string(nil), record.Options.Env...)
	}
	if record.Options.Workdir != "" {
		result.WorkingDir = record.Options.Workdir
	}
	if record.Options.User != "" {
		result.User = record.Options.User
	}
	if len(record.Command) > 0 {
		result.Entrypoint = []string{record.Command[0]}
		result.Cmd = append([]string(nil), record.Command[1:]...)
	}
	if record.Options.StopSignal != "" {
		result.StopSignal = record.Options.StopSignal
	}
	if record.Options.Healthcheck != nil {
		result.Healthcheck = record.Options.Healthcheck
	}
	if len(record.Options.ExposedPorts) > 0 {
		result.ExposedPorts = make(map[string]struct{}, len(record.Options.ExposedPorts))
		for _, value := range record.Options.ExposedPorts {
			result.ExposedPorts[value] = struct{}{}
		}
	}
	return result
}

func applyDockerCommitOverrides(config *imageStore.ImageRuntimeConfig, override imageStore.ImageRuntimeConfig) {
	if override.Env != nil {
		config.Env = append([]string(nil), override.Env...)
	}
	if override.Entrypoint != nil {
		config.Entrypoint = append([]string(nil), override.Entrypoint...)
	}
	if override.Cmd != nil {
		config.Cmd = append([]string(nil), override.Cmd...)
	}
	if override.WorkingDir != "" {
		config.WorkingDir = override.WorkingDir
	}
	if override.User != "" {
		config.User = override.User
	}
	if override.Labels != nil {
		config.Labels = make(map[string]string, len(override.Labels))
		for key, value := range override.Labels {
			config.Labels[key] = value
		}
	}
	if override.Volumes != nil {
		config.Volumes = override.Volumes
	}
	if override.ExposedPorts != nil {
		config.ExposedPorts = override.ExposedPorts
	}
	if override.StopSignal != "" {
		config.StopSignal = override.StopSignal
	}
	if override.Healthcheck != nil {
		config.Healthcheck = override.Healthcheck
	}
}

func dockerCommitExcludedMounts(record state.Record, config imageStore.ImageRuntimeConfig) (map[string]struct{}, error) {
	excluded := make(map[string]struct{})
	add := func(target string) error {
		relative, err := cleanDockerArchivePath(target)
		if err != nil {
			return fmt.Errorf("invalid volume mount target %q: %w", target, err)
		}
		if relative == "" {
			return fmt.Errorf("refusing to exclude the container filesystem root as a mount")
		}
		excluded[relative] = struct{}{}
		return nil
	}
	for _, mount := range record.Options.BindMounts {
		if err := add(mount.Target); err != nil {
			return nil, err
		}
	}
	for _, mount := range record.Options.TmpfsMounts {
		if err := add(mount.Target); err != nil {
			return nil, err
		}
	}
	if record.Options.MountProc {
		if err := add("/proc"); err != nil {
			return nil, err
		}
	}
	for target := range config.Volumes {
		if err := add(target); err != nil {
			return nil, err
		}
	}
	return excluded, nil
}
