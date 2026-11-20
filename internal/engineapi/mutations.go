package engineapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/config"
	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	boxrnetwork "github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/registry"
	"github.com/manit6752025/boxr/internal/state"
)

const maxCreateBodyBytes = 1 << 20

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

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerRestartPolicyConfig struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

const maxDockerRegistryAuthBytes = 16 << 10

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
	if reference == "" || strings.ContainsAny(reference, "\x00\r\n ") || strings.Contains(reference, "@") {
		return "", fmt.Errorf("image must be a literal tagged image reference")
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
	if strings.ContainsAny(fromImage, "\x00\r\n ") || strings.Contains(fromImage, "@") {
		return "", "", "", fmt.Errorf("fromImage must be a literal tagged image reference; digest pulls are not supported yet")
	}
	tag, err := singleQueryValue(query, "tag")
	if err != nil {
		return "", "", "", err
	}
	if _, present := query["tag"]; present && strings.TrimSpace(tag) == "" {
		return "", "", "", fmt.Errorf("tag query parameter must not be empty")
	}
	remoteReference = fromImage
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
	platform, err = singleQueryValue(query, "platform")
	if err != nil {
		return "", "", "", err
	}
	if strings.ContainsAny(platform, "\x00\r\n ") {
		return "", "", "", fmt.Errorf("platform must be linux/ARCH[/VARIANT]")
	}
	return localReference, localReference, platform, nil
}

func writeImagePullStreamError(encoder *json.Encoder, err error) {
	message := err.Error()
	_ = encoder.Encode(map[string]any{
		"error": message, "errorDetail": map[string]string{"message": message},
	})
}

func dockerImageTagTarget(source string, query url.Values) (string, error) {
	repository, err := singleQueryValue(query, "repo")
	if err != nil { return "", err }
	tag, err := singleQueryValue(query, "tag")
	if err != nil { return "", err }
	if tag == "" { tag = "latest" }
	if strings.ContainsAny(repository+tag, "\x00\r\n ") {
		return "", fmt.Errorf("repo and tag must be literal image reference components")
	}
	if repository == "" {
		repository = source
		lastSlash, lastColon := strings.LastIndex(repository, "/"), strings.LastIndex(repository, ":")
		if lastColon > lastSlash { repository = repository[:lastColon] }
	}
	target, err := normalizeDockerLocalImageReference(repository + ":" + tag)
	if err != nil { return "", fmt.Errorf("invalid target image reference: %w", err) }
	return target, nil
}

func (s *Server) handleImageTag(writer http.ResponseWriter, request *http.Request, rawSource string) {
	source, err := decodeDockerImageReference(rawSource)
	if err != nil { writeError(writer, http.StatusBadRequest, err); return }
	target, err := dockerImageTagTarget(source, request.URL.Query())
	if err != nil { writeError(writer, http.StatusBadRequest, err); return }
	if _, err := imageStore.Inspect(source); err != nil {
		if strings.Contains(err.Error(), "not found") { writeError(writer, http.StatusNotFound, err) } else { writeError(writer, http.StatusInternalServerError, err) }
		return
	}
	if _, err := imageStore.Verify(source); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("verify source image: %w", err)); return
	}
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	if target != source {
		records, err := state.List(true)
		if err != nil { writeError(writer, http.StatusInternalServerError, err); return }
		for _, record := range records {
			if record.Labels["io.boxr.image"] == target {
				writeError(writer, http.StatusConflict, fmt.Errorf("target image tag %q is referenced by container %s", target, record.ID))
				return
			}
		}
	}
	if err := imageStore.Tag(source, target); err != nil {
		if strings.Contains(err.Error(), "not found") { writeError(writer, http.StatusNotFound, err) } else { writeError(writer, http.StatusConflict, err) }
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

func createContainerRecord(name string, input dockerCreateRequest) (state.Record, error) {
	var empty state.Record
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
	if len(input.Volumes) > 0 {
		return empty, fmt.Errorf("Config.Volumes and automatic anonymous volumes are not supported; use a pre-created Boxr volume through the native deployment workflow")
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
	if validationOptions.ReadOnlyRootFS || len(validationOptions.BindMounts) > 0 {
		validationOptions.RootFS = "/boxr-engine-api-create-validation"
	}
	if err := validationOptions.Validate(); err != nil {
		return empty, fmt.Errorf("invalid Docker HostConfig: %w", err)
	}
	record, err := state.Create(name, command, options, options.Labels)
	if err != nil {
		return empty, fmt.Errorf("create Boxr container record: %w", err)
	}
	return record, nil
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
	if host.Privileged || host.PublishAllPorts || len(host.CapAdd) > 0 || len(host.Devices) > 0 || len(host.SecurityOpt) > 0 || len(host.Tmpfs) > 0 || len(host.Mounts) > 0 || host.ShmSize > 0 {
		return options, fmt.Errorf("Privileged, CapAdd, Devices, SecurityOpt, Tmpfs, Mounts, ShmSize and PublishAllPorts are not supported by Engine API create")
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
	binds := make([]container.BindMount, 0, len(host.Binds))
	for index, value := range host.Binds {
		mount, err := parseDockerBind(value)
		if err != nil {
			return options, fmt.Errorf("HostConfig.Binds[%d]: %w", index, err)
		}
		binds = append(binds, mount)
	}
	options.BindMounts = binds
	ports, err := parseDockerPortBindings(host.PortBindings)
	if err != nil {
		return options, err
	}
	options.PublishedPorts = ports
	return options, nil
}

func parseDockerBind(value string) (container.BindMount, error) {
	var result container.BindMount
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return result, fmt.Errorf("expected HOST_PATH:CONTAINER_PATH[:ro|rw]")
	}
	if !filepath.IsAbs(parts[0]) || !filepath.IsAbs(parts[1]) || filepath.Clean(parts[1]) == "/" {
		return result, fmt.Errorf("source and target must be absolute paths, and target cannot be /")
	}
	result.Source = filepath.Clean(parts[0])
	result.Target = filepath.Clean(parts[1])
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			result.ReadOnly = true
		case "rw":
		default:
			return result, fmt.Errorf("mount mode must be ro or rw")
		}
	}
	if err := validateDockerBindSource(result.Source); err != nil {
		return result, err
	}
	for _, part := range strings.Split(filepath.ToSlash(result.Target), "/") {
		if part == ".." {
			return result, fmt.Errorf("target path cannot contain '..'")
		}
	}
	return result, nil
}

func validateDockerBindSource(path string) error {
	if path == string(filepath.Separator) {
		return fmt.Errorf("bind source cannot be the host filesystem root")
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect bind source %s: %w", current, err)
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
	options.OverlayDir = ""
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
	if record.Status != "created" {
		writeError(writer, http.StatusConflict, fmt.Errorf("container must be in created state before start (current state: %s)", record.Status))
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
	command := exec.Command(executable,
		"run", "--config", configPath, "--image", image,
		"--detach", "--internal-existing-id", record.ID,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
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
