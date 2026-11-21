package engineapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/config"
	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	boxrnetwork "github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/registry"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

const maxCreateBodyBytes = 1 << 20

var launchEngineAPICreatedContainer = func(executable string, args []string) (stdout, stderr string, err error) {
	command := exec.Command(executable, args...)
	var stdoutBuffer, stderrBuffer bytes.Buffer
	command.Stdout, command.Stderr = &stdoutBuffer, &stderrBuffer
	err = command.Run()
	return stdoutBuffer.String(), stderrBuffer.String(), err
}

type dockerCreateRequest struct {
	Image        string                     `json:"Image"`
	Cmd          []string                   `json:"Cmd"`
	Env          []string                   `json:"Env"`
	Labels       map[string]string          `json:"Labels"`
	WorkingDir   string                     `json:"WorkingDir"`
	Hostname     string                     `json:"Hostname"`
	User         string                     `json:"User"`
	Entrypoint   []string                   `json:"Entrypoint"`
	Tty          bool                       `json:"Tty"`
	OpenStdin    bool                       `json:"OpenStdin"`
	StdinOnce    bool                       `json:"StdinOnce"`
	AttachStdin  bool                       `json:"AttachStdin"`
	AttachStdout bool                       `json:"AttachStdout"`
	AttachStderr bool                       `json:"AttachStderr"`
	Volumes      map[string]json.RawMessage `json:"Volumes"`
	Healthcheck  json.RawMessage            `json:"Healthcheck"`
	StopSignal   string                     `json:"StopSignal"`
	ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
	HostConfig   dockerHostConfig           `json:"HostConfig"`
}

type dockerHostConfig struct {
	Memory            int64                          `json:"Memory"`
	MemoryReservation int64                          `json:"MemoryReservation"`
	MemorySwap        int64                          `json:"MemorySwap"`
	NanoCPUs          int64                          `json:"NanoCpus"`
	PidsLimit         int64                          `json:"PidsLimit"`
	NetworkMode       string                         `json:"NetworkMode"`
	Binds             []string                       `json:"Binds"`
	PortBindings      map[string][]dockerPortBinding `json:"PortBindings"`
	Privileged        bool                           `json:"Privileged"`
	ReadonlyRootfs    bool                           `json:"ReadonlyRootfs"`
	RestartPolicy     dockerRestartPolicyConfig      `json:"RestartPolicy"`
	AutoRemove        bool                           `json:"AutoRemove"`
	CapAdd            []string                       `json:"CapAdd"`
	CapDrop           []string                       `json:"CapDrop"`
	Devices           []json.RawMessage              `json:"Devices"`
	SecurityOpt       []string                       `json:"SecurityOpt"`
	Tmpfs             map[string]string              `json:"Tmpfs"`
	Mounts            []json.RawMessage              `json:"Mounts"`
	ShmSize           int64                          `json:"ShmSize"`
	PublishAllPorts   bool                           `json:"PublishAllPorts"`
}

type dockerMountBindOptions struct {
	Propagation            string `json:"Propagation"`
	CreateMountpoint       bool   `json:"CreateMountpoint"`
	NonRecursive           bool   `json:"NonRecursive"`
	ReadOnlyNonRecursive   bool   `json:"ReadOnlyNonRecursive"`
	ReadOnlyForceRecursive bool   `json:"ReadOnlyForceRecursive"`
}

type dockerMountVolumeDriverConfig struct {
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
}

type dockerMountVolumeOptions struct {
	NoCopy       bool                           `json:"NoCopy"`
	Labels       map[string]string              `json:"Labels"`
	Subpath      string                         `json:"Subpath"`
	DriverConfig *dockerMountVolumeDriverConfig `json:"DriverConfig"`
}

type dockerMountTmpfsOptions struct {
	SizeBytes int64      `json:"SizeBytes"`
	Mode      *uint32    `json:"Mode"`
	Options   [][]string `json:"Options"`
}

type dockerMount struct {
	Type          string                    `json:"Type"`
	Source        string                    `json:"Source"`
	Target        string                    `json:"Target"`
	ReadOnly      bool                      `json:"ReadOnly"`
	Consistency   string                    `json:"Consistency"`
	BindOptions   *dockerMountBindOptions   `json:"BindOptions"`
	VolumeOptions *dockerMountVolumeOptions `json:"VolumeOptions"`
	TmpfsOptions  *dockerMountTmpfsOptions  `json:"TmpfsOptions"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerRestartPolicyConfig struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

const (
	maxDockerRegistryAuthBytes           = 16 << 10
	maxDockerEngineMountCount            = 64
	maxDockerEngineTmpfsBytes      int64 = 1 << 30
	maxDockerEngineTmpfsTotalBytes int64 = 2 << 30
)

func decodeDockerRegistryCredentials(request *http.Request) (registry.Credentials, error) {
	header := strings.TrimSpace(request.Header.Get("X-Registry-Auth"))
	if header == "" {
		return registry.Credentials{}, nil
	}
	if len(header) > maxDockerRegistryAuthBytes {
		return registry.Credentials{}, fmt.Errorf("X-Registry-Auth exceeds %d bytes", maxDockerRegistryAuthBytes)
	}
	var decoded []byte
	var decodeErr error
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		decoded, decodeErr = encoding.DecodeString(header)
		if decodeErr == nil {
			break
		}
	}
	if decodeErr != nil {
		return registry.Credentials{}, fmt.Errorf("X-Registry-Auth is not valid base64")
	}
	var auth map[string]string
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	if err := decoder.Decode(&auth); err != nil {
		return registry.Credentials{}, fmt.Errorf("decode X-Registry-Auth: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return registry.Credentials{}, fmt.Errorf("X-Registry-Auth must contain one JSON object")
	}
	for key := range auth {
		switch key {
		case "username", "password", "auth", "identitytoken", "registrytoken", "email", "serveraddress":
		default:
			return registry.Credentials{}, fmt.Errorf("unsupported X-Registry-Auth field %q", key)
		}
	}
	var credentials registry.Credentials
	if encoded := auth["auth"]; encoded != "" {
		var pair []byte
		var err error
		for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding} {
			pair, err = encoding.DecodeString(encoded)
			if err == nil {
				break
			}
		}
		if err != nil {
			return registry.Credentials{}, fmt.Errorf("X-Registry-Auth auth field is not valid base64")
		}
		username, password, ok := strings.Cut(string(pair), ":")
		if !ok || username == "" {
			return registry.Credentials{}, fmt.Errorf("X-Registry-Auth auth field must encode username:password")
		}
		credentials = registry.Credentials{Username: username, Password: password}
	}
	if auth["username"] != "" || auth["password"] != "" {
		if auth["username"] == "" {
			return registry.Credentials{}, fmt.Errorf("X-Registry-Auth password requires a username")
		}
		explicit := registry.Credentials{Username: auth["username"], Password: auth["password"]}
		if credentials.Username != "" && credentials != explicit {
			return registry.Credentials{}, fmt.Errorf("X-Registry-Auth username/password conflicts with auth field")
		}
		credentials = explicit
	}
	token := auth["identitytoken"]
	if token == "" {
		token = auth["registrytoken"]
	}
	if token != "" {
		if credentials.Username != "" || credentials.Password != "" {
			return registry.Credentials{}, fmt.Errorf("X-Registry-Auth token cannot be combined with username/password")
		}
		credentials = registry.Credentials{Username: "oauth2accesstoken", Password: token}
	}
	return credentials, nil
}

func normalizeDockerLocalImageReference(reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || strings.ContainsAny(reference, "\x00\r\n ") {
		return "", fmt.Errorf("image must be a literal tagged or digest-pinned image reference")
	}
	if strings.Contains(reference, "@") {
		parsed, err := registry.ParseReference(reference)
		if err != nil {
			return "", err
		}
		if parsed.Digest == "" {
			return "", fmt.Errorf("image digest must be an immutable SHA-256 digest")
		}
		return parsed.LocalReference(), nil
	}
	if err := imageStore.ValidateReference(reference); err == nil {
		return reference, nil
	}
	lastSlash := strings.LastIndex(reference, "/")
	lastColon := strings.LastIndex(reference, ":")
	if lastColon <= lastSlash {
		reference += ":latest"
	}
	if err := imageStore.ValidateReference(reference); err != nil {
		return "", err
	}
	return reference, nil
}

func dockerPullReference(query url.Values) (remoteReference, localReference, platform string, err error) {
	fromImage, err := singleQueryValue(query, "fromImage")
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(fromImage) == "" {
		return "", "", "", fmt.Errorf("fromImage query parameter is required")
	}
	if strings.ContainsAny(fromImage, "\x00\r\n ") {
		return "", "", "", fmt.Errorf("fromImage must be a literal image reference")
	}
	tag, err := singleQueryValue(query, "tag")
	if err != nil {
		return "", "", "", err
	}
	if _, present := query["tag"]; present && strings.TrimSpace(tag) == "" {
		return "", "", "", fmt.Errorf("tag query parameter must not be empty")
	}

	remoteReference = fromImage
	switch {
	case strings.Contains(fromImage, "@"):
		if tag != "" {
			return "", "", "", fmt.Errorf("tag cannot be combined with a digest-pinned fromImage reference")
		}
		parsed, parseErr := registry.ParseReference(remoteReference)
		if parseErr != nil {
			return "", "", "", fmt.Errorf("invalid digest-pinned image reference: %w", parseErr)
		}
		if parsed.Digest == "" {
			return "", "", "", fmt.Errorf("image digest must be an immutable SHA-256 digest")
		}
		localReference = parsed.LocalReference()
	case strings.HasPrefix(tag, "sha256:"):
		// Docker clients may place an immutable digest in the tag query
		// parameter instead of appending it to fromImage with '@'.
		name := fromImage
		lastSlash := strings.LastIndex(name, "/")
		lastColon := strings.LastIndex(name, ":")
		if lastColon > lastSlash {
			name = name[:lastColon]
		}
		remoteReference = name + "@" + tag
		parsed, parseErr := registry.ParseReference(remoteReference)
		if parseErr != nil {
			return "", "", "", fmt.Errorf("invalid digest-pinned image reference: %w", parseErr)
		}
		localReference = parsed.LocalReference()
	default:
		if tag != "" {
			lastSlash := strings.LastIndex(remoteReference, "/")
			lastColon := strings.LastIndex(remoteReference, ":")
			if lastColon > lastSlash {
				remoteReference = remoteReference[:lastColon]
			}
			remoteReference += ":" + tag
		}
		localReference, err = normalizeDockerLocalImageReference(remoteReference)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid image reference: %w", err)
		}
		if _, err := registry.ParseReference(localReference); err != nil {
			return "", "", "", err
		}
		remoteReference = localReference
	}

	platform, err = singleQueryValue(query, "platform")
	if err != nil {
		return "", "", "", err
	}
	if strings.ContainsAny(platform, "\x00\r\n ") {
		return "", "", "", fmt.Errorf("platform must be linux/ARCH[/VARIANT]")
	}
	return remoteReference, localReference, platform, nil
}

func writeImagePullStreamError(encoder *json.Encoder, err error) {
	message := err.Error()
	_ = encoder.Encode(map[string]any{
		"error": message, "errorDetail": map[string]string{"message": message},
	})
}

func dockerImageTagTarget(source string, query url.Values) (string, error) {
	repository, err := singleQueryValue(query, "repo")
	if err != nil {
		return "", err
	}
	tag, err := singleQueryValue(query, "tag")
	if err != nil {
		return "", err
	}
	if tag == "" {
		tag = "latest"
	}
	if strings.ContainsAny(repository+tag, "\x00\r\n ") {
		return "", fmt.Errorf("repo and tag must be literal image reference components")
	}
	if repository == "" {
		repository = source
		lastSlash, lastColon := strings.LastIndex(repository, "/"), strings.LastIndex(repository, ":")
		if lastColon > lastSlash {
			repository = repository[:lastColon]
		}
	}
	target, err := normalizeDockerLocalImageReference(repository + ":" + tag)
	if err != nil {
		return "", fmt.Errorf("invalid target image reference: %w", err)
	}
	return target, nil
}

func (s *Server) handleImageTag(writer http.ResponseWriter, request *http.Request, rawSource string) {
	source, err := decodeDockerImageReference(rawSource)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	target, err := dockerImageTagTarget(source, request.URL.Query())
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if _, err := imageStore.Inspect(source); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusInternalServerError, err)
		}
		return
	}
	if _, err := imageStore.Verify(source); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("verify source image: %w", err))
		return
	}
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	if target != source {
		records, err := state.List(true)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err)
			return
		}
		for _, record := range records {
			if record.Labels["io.boxr.image"] == target {
				writeError(writer, http.StatusConflict, fmt.Errorf("target image tag %q is referenced by container %s", target, record.ID))
				return
			}
		}
	}
	if err := imageStore.Tag(source, target); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusConflict, err)
		}
		return
	}
	writer.WriteHeader(http.StatusCreated)
}

func dockerPushReference(rawReference string, query url.Values) (string, error) {
	reference, err := decodeDockerImageReference(rawReference)
	if err != nil {
		return "", err
	}
	tag, err := singleQueryValue(query, "tag")
	if err != nil {
		return "", err
	}
	if _, present := query["tag"]; present && strings.TrimSpace(tag) == "" {
		return "", fmt.Errorf("tag query parameter must not be empty")
	}
	if tag != "" {
		lastSlash := strings.LastIndex(reference, "/")
		lastColon := strings.LastIndex(reference, ":")
		if lastColon > lastSlash {
			reference = reference[:lastColon]
		}
		reference += ":" + tag
	}
	if err := imageStore.ValidateReference(reference); err != nil {
		return "", err
	}
	if _, err := registry.ParseReference(reference); err != nil {
		return "", err
	}
	return reference, nil
}

func (s *Server) handleImagePush(writer http.ResponseWriter, request *http.Request, rawReference string) {
	reference, err := dockerPushReference(rawReference, request.URL.Query())
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	fallbackCredentials, err := decodeDockerRegistryCredentials(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	credentials, err := registry.ResolveCredentials(reference, "", fallbackCredentials)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, fmt.Errorf("registry credentials: %w", err))
		return
	}
	bundle, err := imageStore.ExportRegistryBundle(reference)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusConflict, fmt.Errorf("export image for registry push: %w", err))
		}
		return
	}
	defer bundle.Cleanup()

	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(writer)
	_ = encoder.Encode(map[string]string{"status": "Preparing", "id": reference})
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	_ = encoder.Encode(map[string]string{"status": "Pushing", "id": reference})
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	if err := registry.Push(request.Context(), reference, registry.PushBundle{
		Manifest: bundle.Manifest, Config: bundle.Config, LayerPath: bundle.LayerPath,
	}, credentials); err != nil {
		writeImagePullStreamError(encoder, fmt.Errorf("push image %s: %w", reference, err))
		return
	}
	_ = encoder.Encode(map[string]string{"status": "Push complete", "id": reference})
}

func (s *Server) handleImageCreate(writer http.ResponseWriter, request *http.Request) {
	remoteReference, localReference, platform, err := dockerPullReference(request.URL.Query())
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	fallbackCredentials, err := decodeDockerRegistryCredentials(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	credentials, err := registry.ResolveCredentials(remoteReference, "", fallbackCredentials)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, fmt.Errorf("registry credentials: %w", err))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(writer)
	_ = encoder.Encode(map[string]string{"status": "Pulling image", "id": remoteReference})
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}

	artifact, err := registry.Pull(request.Context(), remoteReference, platform, credentials)
	if err != nil {
		writeImagePullStreamError(encoder, err)
		return
	}
	defer artifact.Cleanup()
	staging, err := os.MkdirTemp("", "boxr-engine-image-pull-*")
	if err != nil {
		writeImagePullStreamError(encoder, fmt.Errorf("create image staging directory: %w", err))
		return
	}
	defer os.RemoveAll(staging)
	rootPath := filepath.Join(staging, "rootfs")
	if err := os.Mkdir(rootPath, 0700); err != nil {
		writeImagePullStreamError(encoder, fmt.Errorf("create image rootfs: %w", err))
		return
	}
	for index, layer := range artifact.Layers {
		if err := imageStore.ApplyLayerArchive(rootPath, layer.Path); err != nil {
			writeImagePullStreamError(encoder, fmt.Errorf("apply image layer %d: %w", index+1, err))
			return
		}
	}
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	records, err := state.List(true)
	if err != nil {
		writeImagePullStreamError(encoder, fmt.Errorf("check existing image users: %w", err))
		return
	}
	for _, record := range records {
		if record.Labels["io.boxr.image"] == localReference {
			writeImagePullStreamError(encoder, fmt.Errorf("image %q is referenced by container %s; refusing to replace a tag in use", localReference, record.ID))
			return
		}
	}
	imported, err := imageStore.ImportRootFSWithConfig(
		rootPath, localReference, artifact.OS, artifact.Architecture, artifact.Variant,
		imageStore.ImageRuntimeConfig{
			Env: artifact.RuntimeConfig.Env, Entrypoint: artifact.RuntimeConfig.Entrypoint,
			Cmd: artifact.RuntimeConfig.Cmd, WorkingDir: artifact.RuntimeConfig.WorkingDir,
			User: artifact.RuntimeConfig.User, Labels: artifact.RuntimeConfig.Labels,
		},
	)
	if err != nil {
		writeImagePullStreamError(encoder, fmt.Errorf("import verified registry image: %w", err))
		return
	}
	_ = encoder.Encode(map[string]string{"status": "Pull complete", "id": imported.Reference})
	_ = encoder.Encode(map[string]string{"status": "Digest: " + artifact.ManifestDigest})
	_ = encoder.Encode(map[string]string{"status": "Downloaded newer image for " + imported.Reference})
}

func (s *Server) handleContainerCreate(writer http.ResponseWriter, request *http.Request) {
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()
	s.networkMu.Lock()
	defer s.networkMu.Unlock()

	data, err := io.ReadAll(io.LimitReader(request.Body, maxCreateBodyBytes+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("read create request: %w", err))
		return
	}
	if len(data) == 0 || len(data) > maxCreateBodyBytes {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("create request must be between 1 byte and %d bytes", maxCreateBodyBytes))
		return
	}
	var input dockerCreateRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("decode container create request: %w", err))
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("container create request must contain one JSON object"))
		return
	}
	name, err := singleQueryValue(request.URL.Query(), "name")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	record, err := createContainerRecord(name, input)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	runFile := config.RunFile{
		Version:  1,
		Name:     record.Name,
		Command:  input.Cmd,
		Options:  sanitizeEngineRunOptions(record.Options),
		Metadata: record.Labels,
	}
	configData, err := json.Marshal(runFile)
	if err == nil {
		err = state.SaveEngineConfig(record.ID, configData)
	}
	if err != nil {
		_ = state.RemoveCreated(record.ID)
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("persist private container configuration: %w", err))
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"Id":       record.ID,
		"Warnings": []string{},
	})
}

func createContainerRecord(name string, input dockerCreateRequest) (result state.Record, resultErr error) {
	var empty state.Record
	anonymousVolumeNames := make([]string, 0)
	anonymousVolumesCommitted := false
	defer func() {
		if anonymousVolumesCommitted {
			return
		}
		for index := len(anonymousVolumeNames) - 1; index >= 0; index-- {
			if removeErr := volume.Remove(anonymousVolumeNames[index]); removeErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("roll back anonymous volume %q: %w", anonymousVolumeNames[index], removeErr))
			}
		}
	}()
	if strings.TrimSpace(input.Image) == "" {
		return empty, fmt.Errorf("Image is required; the image must already exist in the local Boxr image store")
	}
	imageReference, err := normalizeDockerLocalImageReference(input.Image)
	if err != nil || strings.Contains(input.Image, "$") {
		if err == nil {
			err = fmt.Errorf("Image must be a literal local image reference")
		}
		return empty, err
	}
	if input.User != "" {
		return empty, fmt.Errorf("Config.User is unsupported because image user switching is not implemented")
	}
	if len(input.Entrypoint) > 0 {
		return empty, fmt.Errorf("Config.Entrypoint overrides are not supported; use the image's configured entrypoint")
	}
	if input.Tty || input.OpenStdin || input.StdinOnce || input.AttachStdin || input.AttachStdout || input.AttachStderr {
		return empty, fmt.Errorf("interactive create/attach settings are not supported; use boxr exec -it after the container starts")
	}
	if len(input.Healthcheck) > 0 && string(input.Healthcheck) != "null" {
		return empty, fmt.Errorf("Docker Healthcheck configuration is not implemented by Engine API create")
	}
	if input.StopSignal != "" && !strings.EqualFold(input.StopSignal, "SIGTERM") && !strings.EqualFold(input.StopSignal, "TERM") {
		return empty, fmt.Errorf("only the default SIGTERM stop signal is supported")
	}
	if strings.TrimSpace(name) != "" {
		name = strings.TrimPrefix(name, "/")
	}
	image, err := imageStore.Inspect(imageReference)
	if err != nil {
		return empty, fmt.Errorf("image %q is not present in the local Boxr image store: %w", imageReference, err)
	}
	if _, err := imageStore.Verify(imageReference); err != nil {
		return empty, fmt.Errorf("verify local image %q: %w", imageReference, err)
	}
	if image.RuntimeConfig.User != "" {
		return empty, fmt.Errorf("image declares User=%q, but Boxr image user switching is not implemented", image.RuntimeConfig.User)
	}
	options, err := dockerOptions(input.HostConfig)
	if err != nil {
		return empty, err
	}
	options.Name = name
	options.Workdir = input.WorkingDir
	if options.Workdir == "" {
		options.Workdir = image.RuntimeConfig.WorkingDir
	}
	options.Hostname = input.Hostname
	options.Env = container.MergeEnvironment(image.RuntimeConfig.Env, input.Env)
	options.Labels = mergeDockerLabels(image.RuntimeConfig.Labels, input.Labels)
	if _, exists := options.Labels["io.boxr.image"]; exists {
		return empty, fmt.Errorf("label io.boxr.image is reserved by Boxr")
	}
	options.Labels["io.boxr.image"] = imageReference
	if err := options.ApplySecurityProfile(); err != nil {
		return empty, err
	}
	if err := options.ApplyResourceProfile(); err != nil {
		return empty, err
	}
	declaredVolumeTargets, err := dockerConfigVolumeTargets(input.Volumes, image.RuntimeConfig.Volumes)
	if err != nil {
		return empty, fmt.Errorf("validate Docker Config.Volumes: %w", err)
	}
	anonymousMounts, createdVolumeNames, err := createDockerConfigVolumes(declaredVolumeTargets, options)
	if err != nil {
		return empty, err
	}
	anonymousVolumeNames = append(anonymousVolumeNames, createdVolumeNames...)
	options.BindMounts = append(options.BindMounts, anonymousMounts...)
	command := append([]string(nil), input.Cmd...)
	if len(image.RuntimeConfig.Entrypoint) > 0 {
		if len(command) == 0 {
			command = append(command, image.RuntimeConfig.Cmd...)
		}
		command = append(append([]string(nil), image.RuntimeConfig.Entrypoint...), command...)
	} else if len(command) == 0 {
		command = append(command, image.RuntimeConfig.Cmd...)
	}
	if len(command) == 0 {
		return empty, fmt.Errorf("image has no default command; provide Config.Cmd")
	}
	for index, part := range command {
		if strings.ContainsRune(part, '\x00') {
			return empty, fmt.Errorf("command argument %d contains a NUL byte", index+1)
		}
	}
	validationOptions := options
	if validationOptions.ReadOnlyRootFS || len(validationOptions.BindMounts) > 0 || len(validationOptions.TmpfsMounts) > 0 {
		validationOptions.RootFS = "/boxr-engine-api-create-validation"
	}
	if err := validationOptions.Validate(); err != nil {
		return empty, fmt.Errorf("invalid Docker HostConfig: %w", err)
	}
	record, err := state.Create(name, command, options, options.Labels)
	if err != nil {
		return empty, fmt.Errorf("create Boxr container record: %w", err)
	}
	rootPath, err := state.ManagedRootFSDir(record.ID)
	if err != nil {
		_ = state.RemoveCreated(record.ID)
		return empty, fmt.Errorf("prepare persistent container rootfs: %w", err)
	}
	if _, err := imageStore.Unpack(imageReference, rootPath); err != nil {
		_ = state.RemoveManagedRootFS(record.ID)
		_ = state.RemoveCreated(record.ID)
		return empty, fmt.Errorf("unpack verified image into persistent container rootfs: %w", err)
	}
	record, err = state.ConfigureManagedRootFS(record.ID, rootPath)
	if err != nil {
		cleanupErr := state.RemoveManagedRootFS(record.ID)
		removeErr := state.RemoveCreated(record.ID)
		return empty, errors.Join(
			fmt.Errorf("configure persistent container rootfs: %w", err),
			cleanupErr,
			removeErr,
		)
	}
	record, err = state.ConfigureManagedOverlay(record.ID)
	if err != nil {
		if removeErr := state.RemoveCreated(record.ID); removeErr != nil {
			return empty, fmt.Errorf("configure persistent container writable layer: %v; roll back container creation: %w", err, removeErr)
		}
		return empty, fmt.Errorf("configure persistent container writable layer: %w", err)
	}
	if err := copyUpDockerVolumes(options.BindMounts, rootPath); err != nil {
		if removeErr := state.RemoveCreated(record.ID); removeErr != nil {
			return empty, fmt.Errorf("copy image data into Docker volumes: %v; roll back container creation: %w", err, removeErr)
		}
		return empty, fmt.Errorf("copy image data into Docker volumes: %w", err)
	}
	anonymousVolumesCommitted = true
	return record, nil
}

// dockerConfigVolumeTargets merges image-declared and create-request volumes.
// Docker represents each Config.Volumes entry as an empty JSON object.
func dockerConfigVolumeTargets(requested map[string]json.RawMessage, imageVolumes map[string]struct{}) ([]string, error) {
	targets := make(map[string]struct{}, len(requested)+len(imageVolumes))
	add := func(rawTarget string) error {
		target, err := validateDockerMountTarget(rawTarget)
		if err != nil {
			return fmt.Errorf("invalid declared volume target %q: %w", rawTarget, err)
		}
		targets[target] = struct{}{}
		if len(targets) > maxDockerEngineMountCount {
			return fmt.Errorf("Docker Config.Volumes supports at most %d unique targets", maxDockerEngineMountCount)
		}
		return nil
	}
	for target, raw := range requested {
		value := bytes.TrimSpace(raw)
		if !bytes.Equal(value, []byte("{}")) && !bytes.Equal(value, []byte("null")) {
			return nil, fmt.Errorf("Config.Volumes entry %q must be an empty object", target)
		}
		if err := add(target); err != nil {
			return nil, err
		}
	}
	for target := range imageVolumes {
		if err := add(target); err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(targets))
	for target := range targets {
		result = append(result, target)
	}
	sort.Strings(result)
	return result, nil
}

// createDockerConfigVolumes prepares anonymous local volumes declared by image
// metadata or the Docker create request. Explicit mounts at the same target
// take precedence, matching Docker's container-create behaviour.
func createDockerConfigVolumes(targets []string, options container.Options) ([]container.BindMount, []string, error) {
	explicitTargets := make(map[string]bool, len(options.BindMounts)+len(options.TmpfsMounts))
	for _, mount := range options.BindMounts {
		explicitTargets[filepath.Clean(mount.Target)] = true
	}
	for _, mount := range options.TmpfsMounts {
		explicitTargets[filepath.Clean(mount.Target)] = true
	}

	var selected []string
	for _, target := range targets {
		if !explicitTargets[filepath.Clean(target)] {
			selected = append(selected, target)
		}
	}
	if len(options.BindMounts)+len(options.TmpfsMounts)+len(selected) > maxDockerEngineMountCount {
		return nil, nil, fmt.Errorf("Docker container supports at most %d combined mounts, including image-declared volumes", maxDockerEngineMountCount)
	}
	candidateMounts := append([]container.BindMount(nil), options.BindMounts...)
	for _, target := range selected {
		candidateMounts = append(candidateMounts, container.BindMount{
			Source: "/boxr-anonymous-volume-validation", Target: target,
		})
	}
	if err := validateUniqueDockerMountTargets(candidateMounts, options.TmpfsMounts); err != nil {
		return nil, nil, fmt.Errorf("validate Docker declared volume mounts: %w", err)
	}

	mounts := make([]container.BindMount, 0, len(selected))
	created := make([]string, 0, len(selected))
	for _, target := range selected {
		item, err := volume.CreateAnonymous(nil)
		if err != nil {
			var cleanupErr error
			for index := len(created) - 1; index >= 0; index-- {
				cleanupErr = errors.Join(cleanupErr, volume.Remove(created[index]))
			}
			return nil, nil, errors.Join(fmt.Errorf("create anonymous volume for %s: %w", target, err), cleanupErr)
		}
		created = append(created, item.Name)
		mounts = append(mounts, container.BindMount{
			Source: item.Path, Target: target, VolumeName: item.Name, CopyUp: true,
		})
	}
	return mounts, created, nil
}

// copyUpDockerVolumes applies Docker's default initial-volume population only
// to empty, Boxr-managed volumes. A live Boxr container may be writing to a
// volume outside the volume package's mutex, so refuse copy-up while one is
// using that path.
func copyUpDockerVolumes(mounts []container.BindMount, rootPath string) error {
	var selected []container.BindMount
	for _, mount := range mounts {
		if mount.VolumeName != "" && mount.CopyUp {
			selected = append(selected, mount)
		}
	}
	if len(selected) == 0 {
		return nil
	}

	records, err := state.List(true)
	if err != nil {
		return fmt.Errorf("inspect existing container volume users: %w", err)
	}
	// Validate every volume before changing any of them. A conflict on a later
	// mount must not leave earlier empty volumes partially initialised.
	for _, mount := range selected {
		for _, record := range records {
			if record.Status != "running" && record.Status != "restarting" {
				continue
			}
			for _, activeMount := range record.Options.BindMounts {
				if filepath.Clean(activeMount.Source) == filepath.Clean(mount.Source) {
					return fmt.Errorf("volume %q is already in use by running container %s; refusing initial copy-up", mount.VolumeName, record.ID)
				}
			}
		}
	}
	for _, mount := range selected {
		if err := volume.CopyUpFromRootFS(mount.VolumeName, rootPath, mount.Target); err != nil {
			return fmt.Errorf("copy image path %s into volume %q: %w", mount.Target, mount.VolumeName, err)
		}
	}
	return nil
}

func dockerOptions(host dockerHostConfig) (container.Options, error) {
	options := container.Options{
		NetworkMode:       "none",
		MemoryLimitBytes:  host.Memory,
		MemoryLowBytes:    host.MemoryReservation,
		PidsLimit:         host.PidsLimit,
		ReadOnlyRootFS:    host.ReadonlyRootfs,
		AutoRemove:        host.AutoRemove,
		RestartPolicy:     host.RestartPolicy.Name,
		RestartMaxRetries: host.RestartPolicy.MaximumRetryCount,
		DropCapabilities:  append([]string(nil), host.CapDrop...),
	}
	if host.Memory < 0 || host.MemoryReservation < 0 || host.MemorySwap < 0 || host.NanoCPUs < 0 || host.PidsLimit < 0 {
		return options, fmt.Errorf("Docker memory, CPU and PID limits must not be negative")
	}
	if host.NanoCPUs > 0 {
		options.CPUs = float64(host.NanoCPUs) / 1e9
		if math.IsInf(options.CPUs, 0) || math.IsNaN(options.CPUs) {
			return options, fmt.Errorf("NanoCpus is outside the supported range")
		}
	}
	if host.MemorySwap > 0 {
		if host.Memory == 0 {
			return options, fmt.Errorf("MemorySwap requires a positive Memory limit so Boxr can derive the swap-only cgroup limit")
		}
		if host.MemorySwap < host.Memory {
			return options, fmt.Errorf("MemorySwap cannot be smaller than Memory")
		}
		options.MemorySwapMaxBytes = host.MemorySwap - host.Memory
	}
	if host.Privileged || host.PublishAllPorts || len(host.CapAdd) > 0 || len(host.Devices) > 0 || len(host.SecurityOpt) > 0 || len(host.Tmpfs) > 0 || host.ShmSize > 0 {
		return options, fmt.Errorf("Privileged, CapAdd, Devices, SecurityOpt, legacy Tmpfs, ShmSize and PublishAllPorts are not supported by Engine API create")
	}
	if host.RestartPolicy.Name == "" {
		options.RestartPolicy = "no"
	}
	switch options.RestartPolicy {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return options, fmt.Errorf("unsupported RestartPolicy.Name %q", options.RestartPolicy)
	}
	if options.RestartMaxRetries < 0 || options.RestartMaxRetries > 100000 {
		return options, fmt.Errorf("RestartPolicy.MaximumRetryCount must be between 0 and 100000")
	}
	if options.AutoRemove && options.RestartPolicy != "no" {
		return options, fmt.Errorf("AutoRemove cannot be combined with a restart policy")
	}
	mode := strings.TrimSpace(host.NetworkMode)
	switch mode {
	case "", "none":
		options.NetworkMode = "none"
	case "host":
		options.NetworkMode = "host"
	case "bridge":
		if _, err := boxrnetwork.Inspect("bridge"); err != nil {
			return options, fmt.Errorf("Docker bridge mode requires a pre-created Boxr network named bridge: %w", err)
		}
		options.NetworkMode, options.NetworkName = "bridge", "bridge"
	default:
		if _, err := boxrnetwork.Inspect(mode); err != nil {
			return options, fmt.Errorf("Docker network mode %q is not a pre-created Boxr network: %w", mode, err)
		}
		options.NetworkMode, options.NetworkName = "bridge", mode
	}
	if len(host.Binds)+len(host.Mounts) > maxDockerEngineMountCount {
		return options, fmt.Errorf("Docker create supports at most %d combined HostConfig.Binds and HostConfig.Mounts entries", maxDockerEngineMountCount)
	}
	binds := make([]container.BindMount, 0, len(host.Binds))
	for index, value := range host.Binds {
		mount, err := parseDockerBind(value)
		if err != nil {
			return options, fmt.Errorf("HostConfig.Binds[%d]: %w", index, err)
		}
		binds = append(binds, mount)
	}
	structuredBinds, structuredTmpfs, err := parseDockerMounts(host.Mounts)
	if err != nil {
		return options, err
	}
	binds = append(binds, structuredBinds...)
	options.BindMounts = binds
	options.TmpfsMounts = structuredTmpfs
	if err := validateUniqueDockerMountTargets(options.BindMounts, options.TmpfsMounts); err != nil {
		return options, err
	}
	ports, err := parseDockerPortBindings(host.PortBindings)
	if err != nil {
		return options, err
	}
	options.PublishedPorts = ports
	return options, nil
}

func parseDockerBind(value string) (container.BindMount, error) {
	var empty container.BindMount
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return empty, fmt.Errorf("expected HOST_PATH_OR_VOLUME:CONTAINER_PATH[:ro|rw]")
	}
	readOnly := false
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			readOnly = true
		case "rw":
		default:
			return empty, fmt.Errorf("mount mode must be ro or rw")
		}
	}
	// The legacy colon-delimited form historically cleaned its source path
	// before validation. Structured Mounts use parseDockerBindPaths directly
	// so valid POSIX source paths containing ':' remain unambiguous.
	return parseDockerBindPaths(filepath.Clean(parts[0]), parts[1], readOnly)
}

func parseDockerBindPaths(source, rawTarget string, readOnly bool) (container.BindMount, error) {
	var result container.BindMount
	target, err := validateDockerMountTarget(rawTarget)
	if err != nil {
		return result, err
	}
	result.Target, result.ReadOnly = target, readOnly
	if filepath.IsAbs(source) {
		result.Source = source
		if err := validateDockerBindSource(result.Source); err != nil {
			return result, err
		}
	} else {
		if err := volume.ValidateName(source); err != nil {
			return result, fmt.Errorf("source must be an absolute host path or a pre-created Boxr volume name: %w", err)
		}
		item, err := volume.Get(source)
		if err != nil {
			return result, fmt.Errorf("resolve named volume %q: %w", source, err)
		}
		result.Source = item.Path
		result.VolumeName = source
		result.CopyUp = true
	}
	return result, nil
}
func validateDockerMountTarget(raw string) (string, error) {
	if raw == "" || !filepath.IsAbs(raw) || strings.ContainsAny(raw, "\x00\r\n") {
		return "", fmt.Errorf("mount target must be an absolute path")
	}
	for _, part := range strings.Split(filepath.ToSlash(raw), "/") {
		if part == ".." {
			return "", fmt.Errorf("mount target cannot contain '..'")
		}
	}
	clean := filepath.Clean(raw)
	if clean == "/" {
		return "", fmt.Errorf("mount target cannot be the container root")
	}
	return clean, nil
}

// parseDockerMounts translates only mount behaviours that Boxr's local Linux
// runtime can represent faithfully. Unknown fields and unsupported Docker
// mount options fail closed instead of being silently discarded.
func parseDockerMounts(rawMounts []json.RawMessage) ([]container.BindMount, []container.TmpfsMount, error) {
	if len(rawMounts) > 64 {
		return nil, nil, fmt.Errorf("HostConfig.Mounts supports at most 64 entries")
	}
	binds := make([]container.BindMount, 0, len(rawMounts))
	tmpfs := make([]container.TmpfsMount, 0, len(rawMounts))
	var totalTmpfsBytes int64
	for index, raw := range rawMounts {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, nil, fmt.Errorf("HostConfig.Mounts[%d] must be a JSON object", index)
		}
		var mount dockerMount
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&mount); err != nil {
			return nil, nil, fmt.Errorf("HostConfig.Mounts[%d]: %w", index, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return nil, nil, fmt.Errorf("HostConfig.Mounts[%d] must contain one JSON object", index)
		}
		target, err := validateDockerMountTarget(mount.Target)
		if err != nil {
			return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].Target: %w", index, err)
		}
		if mount.Consistency != "" && mount.Consistency != "default" && mount.Consistency != "consistent" {
			return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].Consistency %q is unsupported", index, mount.Consistency)
		}
		switch mount.Type {
		case "bind":
			if mount.VolumeOptions != nil || mount.TmpfsOptions != nil {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d]: BindOptions cannot be combined with VolumeOptions or TmpfsOptions", index)
			}
			if !filepath.IsAbs(mount.Source) || filepath.Clean(mount.Source) != mount.Source {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].Source must be a clean absolute host path", index)
			}
			if mount.BindOptions != nil {
				options := mount.BindOptions
				if options.CreateMountpoint || options.NonRecursive || options.ReadOnlyNonRecursive || options.ReadOnlyForceRecursive {
					return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].BindOptions requests an unsupported host mount operation", index)
				}
				if options.Propagation != "" && options.Propagation != "rprivate" {
					return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].BindOptions.Propagation supports only rprivate", index)
				}
			}
			parsed, err := parseDockerBindPaths(mount.Source, target, mount.ReadOnly)
			if err != nil {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d]: %w", index, err)
			}
			binds = append(binds, parsed)
		case "volume":
			if mount.BindOptions != nil || mount.TmpfsOptions != nil {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d]: VolumeOptions cannot be combined with BindOptions or TmpfsOptions", index)
			}
			if mount.Source == "" {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].Source must name a pre-created Boxr volume", index)
			}
			if mount.VolumeOptions != nil &&
				(len(mount.VolumeOptions.Labels) > 0 || mount.VolumeOptions.Subpath != "" || mount.VolumeOptions.DriverConfig != nil) {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].VolumeOptions supports only NoCopy", index)
			}
			if err := volume.ValidateName(mount.Source); err != nil {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].Source is not a valid Boxr volume name: %w", index, err)
			}
			if _, err := volume.Get(mount.Source); err != nil {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d] requires a pre-created Boxr volume: %w", index, err)
			}
			parsed, err := parseDockerBindPaths(mount.Source, target, mount.ReadOnly)
			if err != nil {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d]: %w", index, err)
			}
			parsed.CopyUp = mount.VolumeOptions == nil || !mount.VolumeOptions.NoCopy
			binds = append(binds, parsed)
		case "tmpfs":
			if mount.Source != "" {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].Source must be empty for tmpfs mounts", index)
			}
			if mount.ReadOnly {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d]: read-only tmpfs mounts are not supported", index)
			}
			if mount.BindOptions != nil || mount.VolumeOptions != nil {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d]: TmpfsOptions cannot be combined with BindOptions or VolumeOptions", index)
			}
			tmpfsMount := container.TmpfsMount{Target: target, Mode: 01777}
			if mount.TmpfsOptions == nil || mount.TmpfsOptions.SizeBytes <= 0 || mount.TmpfsOptions.SizeBytes > maxDockerEngineTmpfsBytes {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].TmpfsOptions.SizeBytes must be between 1 byte and %d bytes", index, maxDockerEngineTmpfsBytes)
			}
			options := mount.TmpfsOptions
			if options.Mode != nil && *options.Mode > 07777 {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].TmpfsOptions.Mode must be at most 07777", index)
			}
			if len(options.Options) > 0 {
				return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].TmpfsOptions.Options are not supported", index)
			}
			if options.SizeBytes > maxDockerEngineTmpfsTotalBytes-totalTmpfsBytes {
				return nil, nil, fmt.Errorf("combined Docker tmpfs mounts exceed %d bytes", maxDockerEngineTmpfsTotalBytes)
			}
			totalTmpfsBytes += options.SizeBytes
			tmpfsMount.SizeBytes = options.SizeBytes
			if options.Mode != nil {
				tmpfsMount.Mode = *options.Mode
			}
			tmpfs = append(tmpfs, tmpfsMount)
		default:
			return nil, nil, fmt.Errorf("HostConfig.Mounts[%d].Type %q is unsupported (supported: bind, volume, tmpfs)", index, mount.Type)
		}
	}
	return binds, tmpfs, nil
}

func dockerMountTargetContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateUniqueDockerMountTargets(binds []container.BindMount, tmpfs []container.TmpfsMount) error {
	seen := make([]string, 0, len(binds)+len(tmpfs))
	check := func(target string) error {
		for _, previous := range seen {
			if previous == target {
				return fmt.Errorf("multiple Docker mounts target %q", target)
			}
			if dockerMountTargetContains(previous, target) || dockerMountTargetContains(target, previous) {
				return fmt.Errorf("nested Docker mount targets %q and %q are not supported", previous, target)
			}
		}
		seen = append(seen, target)
		return nil
	}
	for _, mount := range binds {
		if err := check(mount.Target); err != nil {
			return err
		}
	}
	for _, mount := range tmpfs {
		if err := check(mount.Target); err != nil {
			return err
		}
	}
	return nil
}

func validateDockerBindSource(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("bind source must be a clean absolute path")
	}
	if path == string(filepath.Separator) || path == filepath.VolumeName(path)+string(filepath.Separator) {
		return fmt.Errorf("bind source cannot be the host filesystem root")
	}

	rootPath := filepath.VolumeName(path) + string(filepath.Separator)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open bind source volume root: %w", err)
	}
	defer root.Close()

	relative := strings.TrimPrefix(path, rootPath)
	parts := strings.Split(relative, string(filepath.Separator))
	walked := make([]string, 0, len(parts))
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("bind source contains an invalid path component")
		}
		walked = append(walked, part)
		current := strings.Join(walked, string(filepath.Separator))
		if !filepath.IsLocal(current) {
			return fmt.Errorf("bind source contains a non-local path component")
		}
		info, err := root.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect bind source component %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bind source contains symlink component %s", current)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("bind source parent %s is not a directory", current)
		}
		if index == len(parts)-1 && !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("bind source must be a regular file or directory")
		}
	}
	return nil
}

func parseDockerPortBindings(input map[string][]dockerPortBinding) ([]container.PortMapping, error) {
	var result []container.PortMapping
	for key, bindings := range input {
		portPart, protocol, hasProtocol := strings.Cut(key, "/")
		if !hasProtocol {
			protocol = "tcp"
		}
		port, err := strconv.ParseUint(portPart, 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("invalid PortBindings key %q", key)
		}
		if protocol != "tcp" && protocol != "udp" {
			return nil, fmt.Errorf("unsupported port protocol %q", protocol)
		}
		for index, binding := range bindings {
			if binding.HostPort == "" {
				return nil, fmt.Errorf("PortBindings[%q][%d] must specify a fixed HostPort; ephemeral host ports are not supported", key, index)
			}
			hostPort, err := strconv.ParseUint(binding.HostPort, 10, 16)
			if err != nil || hostPort == 0 {
				return nil, fmt.Errorf("PortBindings[%q][%d] has an invalid HostPort", key, index)
			}
			mapping := container.PortMapping{
				HostIP: binding.HostIP, HostPort: uint16(hostPort),
				ContainerPort: uint16(port), Protocol: protocol,
			}
			normalized, err := mapping.Normalize()
			if err != nil {
				return nil, fmt.Errorf("PortBindings[%q][%d]: %w", key, index, err)
			}
			result = append(result, normalized)
		}
	}
	return result, nil
}

func mergeDockerLabels(groups ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, group := range groups {
		for key, value := range group {
			result[key] = value
		}
	}
	return result
}

func sanitizeEngineRunOptions(options container.Options) container.Options {
	options.Env = append([]string(nil), options.Env...)
	options.BindMounts = nil
	options.ReadOnlyRootFS = false
	options.MountProc = false
	options.TmpfsMounts = nil
	return options
}

func singleQueryValue(query url.Values, key string) (string, error) {
	values, ok := query[key]
	if !ok {
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("query parameter %s must occur exactly once", key)
	}
	return values[0], nil
}

func (s *Server) handleContainerRestart(writer http.ResponseWriter, request *http.Request, rawID string) {
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Options.AutoRemove {
		writeError(writer, http.StatusConflict, fmt.Errorf("container uses automatic removal and cannot be restarted"))
		return
	}
	rawTimeout, err := singleQueryValue(request.URL.Query(), "t")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	timeout := 10 * time.Second
	if rawTimeout != "" {
		seconds, parseErr := strconv.Atoi(rawTimeout)
		if parseErr != nil || seconds < -1 || seconds > 300 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("restart timeout t must be -1 or between 0 and 300 seconds"))
			return
		}
		if seconds == -1 {
			timeout = 5 * time.Minute
		} else {
			timeout = time.Duration(seconds) * time.Second
		}
	}
	executable, err := os.Executable()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	stdout, stderr, launchErr := launchEngineAPICreatedContainer(executable, []string{
		"restart", "--time", timeout.String(), record.ID,
	})
	if launchErr != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		if message == "" {
			message = launchErr.Error()
		}
		writeError(writer, http.StatusConflict, fmt.Errorf("restart container: %s", message))
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerStart(writer http.ResponseWriter, request *http.Request, rawID string) {
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status == "running" || record.Status == "restarting" {
		writeError(writer, http.StatusNotModified, fmt.Errorf("container is already running"))
		return
	}
	if record.Status != "created" && record.Status != "exited" && record.Status != "failed" {
		writeError(writer, http.StatusConflict, fmt.Errorf("container must be created or stopped before start (current state: %s)", record.Status))
		return
	}
	image := record.Labels["io.boxr.image"]
	if image == "" {
		writeError(writer, http.StatusConflict, fmt.Errorf("container has no saved local image reference"))
		return
	}
	configPath, err := state.EngineConfigPath(record.ID)
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	stdout, stderr, launchErr := launchEngineAPICreatedContainer(executable, []string{
		"run", "--config", configPath, "--image", image,
		"--detach", "--internal-existing-id", record.ID,
	})
	if launchErr != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		if message == "" {
			message = launchErr.Error()
		}
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("start container: %s", message))
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerStop(writer http.ResponseWriter, request *http.Request, rawID string) {
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	timeout := 10 * time.Second
	if raw := request.URL.Query().Get("t"); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < -1 || seconds > 300 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("stop timeout t must be -1 or between 0 and 300 seconds"))
			return
		}
		if seconds == -1 {
			timeout = 5 * time.Minute
		} else {
			timeout = time.Duration(seconds) * time.Second
		}
	}
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status == "restarting" {
		if err := state.RequestStop(record.ID); err != nil {
			writeError(writer, http.StatusConflict, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if record.Status != "running" {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	if err := state.RequestStop(record.ID); err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	signal := "TERM"
	if timeout == 0 {
		signal = "KILL"
	}
	if err := state.Signal(record.ID, signal); err != nil {
		if rollbackErr := state.CancelStopRequest(record.ID); rollbackErr != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("send stop signal: %v; roll back stop request: %w", err, rollbackErr))
			return
		}
		writeError(writer, http.StatusConflict, err)
		return
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, getErr := state.Get(record.ID)
		if getErr != nil {
			writeError(writer, http.StatusInternalServerError, getErr)
			return
		}
		if current.Status != "running" && current.Status != "restarting" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		if !time.Now().Before(deadline) && signal != "KILL" {
			if killErr := state.Signal(record.ID, "KILL"); killErr != nil {
				writeError(writer, http.StatusConflict, killErr)
				return
			}
			signal = "KILL"
			deadline = time.Now().Add(2 * time.Second)
		} else if signal == "KILL" && !time.Now().Before(deadline) {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("container did not stop after SIGKILL"))
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
