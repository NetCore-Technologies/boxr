package engineapi

import (
	"bytes"
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

func (s *Server) handleContainerCreate(writer http.ResponseWriter, request *http.Request) {
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
		return empty, fmt.Errorf("Image is required; Engine API image pull is not implemented, so the image must already exist locally")
	}
	if strings.ContainsAny(input.Image, "\x00\r\n") || strings.Contains(input.Image, "$") {
		return empty, fmt.Errorf("Image must be a literal local image reference")
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
	image, err := imageStore.Inspect(input.Image)
	if err != nil {
		return empty, fmt.Errorf("image %q is not present in the local Boxr image store: %w", input.Image, err)
	}
	if _, err := imageStore.Verify(input.Image); err != nil {
		return empty, fmt.Errorf("verify local image %q: %w", input.Image, err)
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
	options.Labels["io.boxr.image"] = input.Image
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
	if record.Status != "running" && record.Status != "restarting" {
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
