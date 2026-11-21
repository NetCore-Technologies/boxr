package engineapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/rootfs"
	"github.com/manit6752025/boxr/internal/state"
)

const (
	maxDockerBuildContextBytes = int64(8 << 30)
	maxDockerBuildArgsBytes    = int64(1 << 20)
	maxDockerBuildArgsCount    = 256
	maxDockerBuildTagCount     = 128
)

type dockerBuildOptions struct {
	tags       []string
	dockerfile string
	platform   string
	network    string
	quiet      bool
	buildArgs  map[string]string
}

func parseDockerBuildOptions(query url.Values) (dockerBuildOptions, error) {
	options := dockerBuildOptions{
		dockerfile: "Dockerfile",
		network:    "none",
		buildArgs:  make(map[string]string),
	}
	allowed := map[string]struct{}{
		"t": {}, "dockerfile": {}, "platform": {}, "networkmode": {},
		"q": {}, "quiet": {}, "nocache": {}, "pull": {}, "rm": {},
		"forcerm": {}, "buildargs": {}, "version": {},
	}
	for key := range query {
		if _, ok := allowed[key]; !ok {
			return options, fmt.Errorf("unsupported Docker build query parameter %q", key)
		}
	}

	tags := query["t"]
	if len(tags) == 0 || len(tags) > maxDockerBuildTagCount {
		return options, fmt.Errorf("Docker build requires between 1 and %d t tags", maxDockerBuildTagCount)
	}
	seenTags := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			return options, fmt.Errorf("Docker build tag must not be empty")
		}
		if err := imageStore.ValidateReference(tag); err != nil {
			return options, fmt.Errorf("invalid Docker build tag %q: %w", tag, err)
		}
		if _, exists := seenTags[tag]; exists {
			continue
		}
		seenTags[tag] = struct{}{}
		options.tags = append(options.tags, tag)
	}

	dockerfile, err := singleQueryValue(query, "dockerfile")
	if err != nil {
		return options, err
	}
	if dockerfile != "" {
		if strings.ContainsRune(dockerfile, 0) || strings.ContainsRune(dockerfile, 92) {
			return options, fmt.Errorf("Dockerfile path contains invalid characters")
		}
		relative := filepath.FromSlash(dockerfile)
		if !filepath.IsLocal(relative) || relative == "." {
			return options, fmt.Errorf("Dockerfile path must be a local file inside the build context")
		}
		options.dockerfile = filepath.ToSlash(filepath.Clean(relative))
	}

	options.platform, err = singleQueryValue(query, "platform")
	if err != nil {
		return options, err
	}
	networkMode, err := singleQueryValue(query, "networkmode")
	if err != nil {
		return options, err
	}
	if networkMode != "" {
		options.network = networkMode
	}
	if options.network != "none" && options.network != "host" {
		return options, fmt.Errorf("Docker build networkmode must be none or host")
	}

	quietValue, err := singleQueryValue(query, "q")
	if err != nil {
		return options, err
	}
	legacyQuiet, err := singleQueryValue(query, "quiet")
	if err != nil {
		return options, err
	}
	if quietValue != "" && legacyQuiet != "" {
		return options, fmt.Errorf("use only one of q or quiet")
	}
	if quietValue == "" {
		quietValue = legacyQuiet
	}
	if quietValue != "" {
		options.quiet, err = parseBooleanFilter(quietValue)
		if err != nil {
			return options, fmt.Errorf("invalid Docker build quiet flag: %w", err)
		}
	}

	for _, key := range []string{"nocache", "pull", "rm", "forcerm"} {
		value, err := singleQueryValue(query, key)
		if err != nil {
			return options, err
		}
		if value == "" {
			continue
		}
		enabled, err := parseBooleanFilter(value)
		if err != nil {
			return options, fmt.Errorf("invalid Docker build %s flag: %w", key, err)
		}
		switch key {
		case "nocache":
			if enabled {
				return options, fmt.Errorf("nocache=true is not supported; Boxr currently has no Docker build cache")
			}
		case "pull":
			if enabled {
				return options, fmt.Errorf("pull=true is not supported; pull base images explicitly before building")
			}
		case "rm":
			if !enabled {
				return options, fmt.Errorf("rm=false is not supported; Boxr always removes temporary build state")
			}
		}
	}
	version, err := singleQueryValue(query, "version")
	if err != nil {
		return options, err
	}
	if version != "" && version != "1" {
		return options, fmt.Errorf("unsupported Docker build version %q", version)
	}

	buildArgsJSON, err := singleQueryValue(query, "buildargs")
	if err != nil {
		return options, err
	}
	if buildArgsJSON != "" {
		if len(buildArgsJSON) > maxDockerBuildArgsBytes {
			return options, fmt.Errorf("Docker buildargs JSON exceeds %d bytes", maxDockerBuildArgsBytes)
		}
		decoder := json.NewDecoder(bytes.NewBufferString(buildArgsJSON))
		var supplied map[string]*string
		if err := decoder.Decode(&supplied); err != nil {
			return options, fmt.Errorf("decode Docker buildargs: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return options, fmt.Errorf("Docker buildargs must contain one JSON object")
		}
		if len(supplied) > maxDockerBuildArgsCount {
			return options, fmt.Errorf("Docker buildargs may contain at most %d entries", maxDockerBuildArgsCount)
		}
		for key, value := range supplied {
			if strings.TrimSpace(key) == "" || len(key) > 128 || strings.ContainsAny(key, "=\x00\r\n") {
				return options, fmt.Errorf("invalid Docker build argument name %q", key)
			}
			if value == nil {
				continue
			}
			if len(*value) > 64<<10 || strings.ContainsRune(*value, '\x00') {
				return options, fmt.Errorf("Docker build argument %q is too large or contains NUL", key)
			}
			options.buildArgs[key] = *value
		}
	}
	sort.Strings(options.tags)
	return options, nil
}

func (s *Server) handleImageBuild(writer http.ResponseWriter, request *http.Request) {
	options, err := parseDockerBuildOptions(request.URL.Query())
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if request.ContentLength == 0 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("Docker build context archive must not be empty"))
		return
	}

	workspace, err := os.MkdirTemp("", "boxr-engine-build-*")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create Docker build workspace: %w", err))
		return
	}
	defer os.RemoveAll(workspace)

	archivePath := filepath.Join(workspace, "context.tar")
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create Docker build context archive: %w", err))
		return
	}
	written, copyErr := io.Copy(archive, io.LimitReader(request.Body, maxDockerBuildContextBytes+1))
	if copyErr == nil && written == 0 {
		copyErr = fmt.Errorf("Docker build context archive must not be empty")
	}
	if copyErr == nil && written > maxDockerBuildContextBytes {
		copyErr = fmt.Errorf("Docker build context exceeds %d bytes", maxDockerBuildContextBytes)
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
		if written > maxDockerBuildContextBytes {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(writer, status, fmt.Errorf("stage Docker build context: %w", copyErr))
		return
	}

	contextDir := filepath.Join(workspace, "context")
	if _, err := rootfs.ExtractTar(archivePath, contextDir, rootfs.Limits{
		MaxFiles: rootfs.DefaultMaxFiles,
		MaxBytes: rootfs.DefaultMaxBytes,
	}); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("extract Docker build context: %w", err))
		return
	}
	dockerfilePath := filepath.Join(contextDir, filepath.FromSlash(options.dockerfile))

	// Keep tag reference checks, build publication and additional aliases in one
	// critical section so a container cannot be created against a tag that the
	// build is about to replace.
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect image users before build: %w", err))
		return
	}
	for _, record := range records {
		usedReference := record.Labels["io.boxr.image"]
		for _, tag := range options.tags {
			if usedReference == tag {
				writeError(writer, http.StatusConflict, fmt.Errorf("image tag %q is referenced by container %s; refusing to replace it", tag, record.ID))
				return
			}
		}
	}

	built, err := imageStore.BuildDockerfileForPlatformWithOptions(
		contextDir, dockerfilePath, options.tags[0], options.platform, options.network, options.buildArgs,
	)
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("Dockerfile build failed: %w", err))
		return
	}
	for _, tag := range options.tags[1:] {
		if err := imageStore.Tag(built.Reference, tag); err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("tag built image as %q: %w", tag, err))
			return
		}
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	if options.quiet {
		return
	}
	encoder := json.NewEncoder(writer)
	_ = encoder.Encode(map[string]string{"stream": "Successfully built " + dockerImageID(built.Digest) + "\n"})
	for _, tag := range options.tags {
		_ = encoder.Encode(map[string]string{"stream": "Successfully tagged " + tag + "\n"})
	}
	_ = encoder.Encode(map[string]any{"aux": map[string]string{"ID": dockerImageID(built.Digest)}})
}
