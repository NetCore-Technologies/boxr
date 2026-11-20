// Package engineapi exposes Boxr's local runtime state through a conservative
// Docker Engine HTTP API read surface. It deliberately does not claim that
// unsupported Docker mutations or runtime features are implemented.
package engineapi

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

const (
	apiVersion    = "1.45"
	minAPIVersion = "1.24"
)

type Server struct {
	version string
}

func New(version string) *Server {
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	return &Server{version: version}
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("API-Version", apiVersion)
	writer.Header().Set("Docker-Experimental", "false")
	writer.Header().Set("Ostype", "linux")
	writer.Header().Set("Server", "Boxr/"+s.version)

	path, err := stripAPIVersion(request.URL.Path)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if path == "/_ping" {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writeMethodError(writer)
			return
		}
		writer.Header().Set("Content-Type", "text/plain")
		if request.Method == http.MethodGet {
			_, _ = io.WriteString(writer, "OK")
		}
		return
	}
	if path == "/version" {
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"Version": s.version, "ApiVersion": apiVersion, "MinAPIVersion": minAPIVersion,
			"GitCommit": "unknown", "GoVersion": runtime.Version(), "Os": "linux",
			"Arch": runtime.GOARCH, "BuildTime": "unknown", "Experimental": false,
		})
		return
	}
	switch {
	case path == "/info":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleInfo(writer)
	case path == "/containers/json":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleContainers(writer, request)
	case strings.HasPrefix(path, "/containers/"):
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleContainer(writer, request, strings.TrimPrefix(path, "/containers/"))
	case path == "/images/json":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleImages(writer)
	case path == "/events":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleEvents(writer, request)
	default:
		writeError(writer, http.StatusNotFound, fmt.Errorf("page not found"))
	}
}

func stripAPIVersion(path string) (string, error) {
	if path == "" || path[0] != '/' {
		return "", fmt.Errorf("invalid API path")
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	if len(parts) == 0 || len(parts[0]) < 2 || parts[0][0] != 'v' {
		return path, nil
	}
	versionParts := strings.Split(parts[0][1:], ".")
	if len(versionParts) != 2 {
		return path, nil
	}
	major, majorErr := strconv.Atoi(versionParts[0])
	minor, minorErr := strconv.Atoi(versionParts[1])
	if majorErr != nil || minorErr != nil {
		return path, nil
	}
	if major != 1 || minor < 0 {
		return "", fmt.Errorf("unsupported API version %q", parts[0])
	}
	if minor > 45 {
		return "", fmt.Errorf("client API version %s is newer than the maximum supported API version %s", parts[0], apiVersion)
	}
	if minor < 24 {
		return "", fmt.Errorf("client API version %s is older than the minimum supported API version %s", parts[0], minAPIVersion)
	}
	if len(parts) == 1 {
		return "/", nil
	}
	return "/" + parts[1], nil
}

func (s *Server) handleInfo(writer http.ResponseWriter) {
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	images, err := imageStore.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	running, stopped := 0, 0
	for _, record := range records {
		if record.Status == "running" || record.Status == "restarting" {
			running++
		} else {
			stopped++
		}
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "boxr"
	}
	stateDir, _ := state.Dir()
	kernelVersion, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	writeJSON(writer, http.StatusOK, map[string]any{
		"ID": hostname, "Containers": len(records), "ContainersRunning": running,
		"ContainersPaused": 0, "ContainersStopped": stopped, "Images": len(images),
		"Driver": "boxr-rootfs", "DriverStatus": [][]string{}, "DockerRootDir": stateDir,
		"OperatingSystem": "Boxr", "OSType": "linux", "Architecture": runtime.GOARCH,
		"KernelVersion": strings.TrimSpace(string(kernelVersion)), "NCPU": runtime.NumCPU(),
		"ServerVersion": s.version, "SecurityOptions": []string{"namespaces", "seccomp"},
		"LoggingDriver": "boxr-file", "CgroupDriver": "cgroupfs", "CgroupVersion": "2",
	})
}

type dockerPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

func portsForOptions(options container.Options) []dockerPort {
	result := make([]dockerPort, 0, len(options.PublishedPorts))
	for _, mapping := range options.PublishedPorts {
		hostIP := mapping.HostIP
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		protocol := mapping.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		result = append(result, dockerPort{
			IP: hostIP, PrivatePort: mapping.ContainerPort,
			PublicPort: mapping.HostPort, Type: protocol,
		})
	}
	return result
}

func containerState(record state.Record) string {
	switch record.Status {
	case "running", "restarting", "created", "exited", "dead", "paused":
		return record.Status
	case "failed":
		return "exited"
	default:
		return record.Status
	}
}

func containerStatus(record state.Record) string {
	switch containerState(record) {
	case "running":
		return "Up"
	case "restarting":
		return "Restarting"
	case "created":
		return "Created"
	case "exited":
		if record.ExitCode != nil {
			return fmt.Sprintf("Exited (%d)", *record.ExitCode)
		}
		return "Exited"
	case "paused":
		return "Up (Paused)"
	default:
		return "Dead"
	}
}

func displayName(record state.Record) string {
	if record.Name != "" {
		return "/" + record.Name
	}
	return "/" + record.ID
}

func (s *Server) handleContainers(writer http.ResponseWriter, request *http.Request) {
	all, err := boolQuery(request, "all")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		if !all && record.Status != "running" && record.Status != "restarting" {
			continue
		}
		if !matchesContainerFilters(record, filters) {
			continue
		}
		image := record.Labels["io.boxr.image"]
		imageID := ""
		if image != "" {
			if inspected, inspectErr := imageStore.Inspect(image); inspectErr == nil {
				imageID = "sha256:" + inspected.Digest
			}
		}
		command := strings.Join(record.Command, " ")
		item := map[string]any{
			"Id": record.ID, "Names": []string{displayName(record)}, "Image": image,
			"ImageID": imageID, "Command": command, "Created": record.CreatedAt.Unix(),
			"Ports": portsForOptions(record.Options), "Labels": record.Labels,
			"State": containerState(record), "Status": containerStatus(record),
			"HostConfig": map[string]any{"NetworkMode": dockerNetworkMode(record.Options)},
			"NetworkSettings": map[string]any{"Networks": map[string]any{}},
		}
		result = append(result, item)
	}
	limit, err := intQuery(request, "limit")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	writeJSON(writer, http.StatusOK, result)
}

func dockerNetworkMode(options container.Options) string {
	switch options.NetworkMode {
	case "", "none":
		if options.NetworkMode == "" {
			return "none"
		}
		return options.NetworkMode
	case "bridge":
		if options.NetworkName != "" {
			return options.NetworkName
		}
		return "bridge"
	default:
		return options.NetworkMode
	}
}

func parseFilters(raw string) (map[string][]string, error) {
	result := make(map[string][]string)
	if raw == "" {
		return result, nil
	}
	data, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		data, err = base64.RawURLEncoding.DecodeString(raw)
	}
	if err != nil {
		data, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("decode filters: %w", err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode filters JSON: %w", err)
	}
	return result, nil
}

func matchesContainerFilters(record state.Record, filters map[string][]string) bool {
	for key, values := range filters {
		switch key {
		case "status":
			matched := false
			for _, value := range values {
				if value == containerState(record) {
					matched = true
				}
			}
			if !matched {
				return false
			}
		case "name":
			matched := false
			for _, value := range values {
				if strings.Contains(strings.TrimPrefix(displayName(record), "/"), strings.TrimPrefix(value, "/")) {
					matched = true
				}
			}
			if !matched {
				return false
			}
		case "id":
			matched := false
			for _, value := range values {
				if strings.HasPrefix(record.ID, value) {
					matched = true
				}
			}
			if !matched {
				return false
			}
		case "label":
			matched := false
			for _, value := range values {
				key, wanted, hasValue := strings.Cut(value, "=")
				actual, exists := record.Labels[key]
				if exists && (!hasValue || actual == wanted) {
					matched = true
				}
			}
			if !matched {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleContainer(writer http.ResponseWriter, request *http.Request, remainder string) {
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		writeError(writer, http.StatusNotFound, fmt.Errorf("container endpoint not found"))
		return
	}
	query, err := url.PathUnescape(parts[0])
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	switch parts[1] {
	case "json":
		s.handleContainerInspect(writer, query)
	case "logs":
		s.handleContainerLogs(writer, request, query)
	case "stats":
		s.handleContainerStats(writer, request, query)
	default:
		writeError(writer, http.StatusNotFound, fmt.Errorf("container endpoint not found"))
	}
}

func (s *Server) handleContainerInspect(writer http.ResponseWriter, query string) {
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	path := ""
	var commandArgs []string
	if len(record.Command) > 0 {
		path, commandArgs = record.Command[0], append([]string(nil), record.Command[1:]...)
	}
	image := record.Labels["io.boxr.image"]
	startedAt, finishedAt := "", ""
	if record.StartedAt != nil {
		startedAt = record.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if record.FinishedAt != nil {
		finishedAt = record.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	created := record.CreatedAt.UTC().Format(time.RFC3339Nano)
	running := record.Status == "running"
	ports := make(map[string][]map[string]string)
	for _, mapping := range record.Options.PublishedPorts {
		protocol := mapping.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		key := fmt.Sprintf("%d/%s", mapping.ContainerPort, protocol)
		ports[key] = append(ports[key], map[string]string{
			"HostIp": mapping.HostIP, "HostPort": strconv.Itoa(int(mapping.HostPort)),
		})
	}
	mounts := make([]map[string]any, 0, len(record.Options.BindMounts))
	for _, mount := range record.Options.BindMounts {
		mounts = append(mounts, map[string]any{
			"Type": "bind", "Source": mount.Source, "Destination": mount.Target,
			"RW": !mount.ReadOnly,
		})
	}
	stateObject := map[string]any{
		"Status": containerState(record), "Running": running, "Paused": false,
		"Restarting": record.Status == "restarting", "OOMKilled": false, "Dead": record.Status == "dead",
		"Pid": record.PID, "ExitCode": exitCode(record.ExitCode), "Error": record.Error,
		"StartedAt": startedAt, "FinishedAt": finishedAt,
	}
	configObject := map[string]any{
		"Hostname": record.Options.Hostname, "Cmd": record.Command,
		"Image": image, "WorkingDir": record.Options.Workdir, "Labels": record.Labels,
		"OpenStdin": false, "StdinOnce": false, "Tty": false,
	}
	hostConfig := map[string]any{
		"NetworkMode": dockerNetworkMode(record.Options), "Memory": record.Options.MemoryLimitBytes,
		"NanoCpus": int64(record.Options.CPUs * 1e9), "PidsLimit": record.Options.PidsLimit,
		"RestartPolicy": map[string]any{"Name": dockerRestartPolicy(record.Options.RestartPolicy), "MaximumRetryCount": record.Options.RestartMaxRetries},
		"PortBindings": dockerPortBindings(record.Options), "Binds": dockerBinds(record.Options),
		"Privileged": false, "ReadonlyRootfs": record.Options.ReadOnlyRootFS,
	}
	networkSettings := map[string]any{
		"Ports": ports, "Networks": map[string]any{},
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"Id": record.ID, "Created": created, "Path": path, "Args": commandArgs,
		"State": stateObject, "Image": image, "Name": displayName(record),
		"RestartCount": record.RestartCount, "Driver": "boxr-rootfs", "Platform": "linux",
		"Mounts": mounts, "HostConfig": hostConfig,
		"Config": configObject, "NetworkSettings": networkSettings,
	})
}

func exitCode(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func dockerRestartPolicy(value string) string {
	if value == "" {
		return "no"
	}
	return value
}

func dockerPortBindings(options container.Options) map[string][]map[string]string {
	result := make(map[string][]map[string]string)
	for _, mapping := range options.PublishedPorts {
		protocol := mapping.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		key := fmt.Sprintf("%d/%s", mapping.ContainerPort, protocol)
		result[key] = append(result[key], map[string]string{
			"HostIp": mapping.HostIP, "HostPort": strconv.Itoa(int(mapping.HostPort)),
		})
	}
	return result
}

func dockerBinds(options container.Options) []string {
	result := make([]string, 0, len(options.BindMounts))
	for _, mount := range options.BindMounts {
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		result = append(result, mount.Source+":"+mount.Target+":"+mode)
	}
	return result
}

func (s *Server) handleContainerLogs(writer http.ResponseWriter, request *http.Request, query string) {
	stdout, err := boolQuery(request, "stdout")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	stderr, err := boolQuery(request, "stderr")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if !stdout && !stderr {
		write(writer, http.StatusOK, "application/vnd.docker.raw-stream", nil)
		return
	}
	tail := 0
	rawTail := request.URL.Query().Get("tail")
	if rawTail != "" && rawTail != "all" {
		tail, err = strconv.Atoi(rawTail)
		if err != nil || tail < 0 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("tail must be all or a non-negative integer"))
			return
		}
	}
	data, offset, err := state.ReadLogsSnapshot(query, tail)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	writer.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	writer.WriteHeader(http.StatusOK)
	if len(data) > 0 {
		if err := writeLogFrame(writer, data, 1); err != nil {
			return
		}
	}
	follow, err := boolQuery(request, "follow")
	if err != nil || !follow {
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			var buffer strings.Builder
			next, copyErr := state.CopyLogFrom(query, offset, &buffer)
			if copyErr != nil {
				return
			}
			if next > offset {
				if writeLogFrame(writer, []byte(buffer.String()), 1) != nil {
					return
				}
				offset = next
				flusher.Flush()
			}
			record, getErr := state.Get(query)
			if getErr != nil || (record.Status != "running" && record.Status != "restarting") {
				return
			}
		}
	}
}

func writeLogFrame(writer io.Writer, data []byte, stream byte) error {
	for len(data) > 0 {
		length := len(data)
		if length > 16<<20 {
			length = 16 << 20
		}
		var header [8]byte
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(length))
		if _, err := writer.Write(header[:]); err != nil {
			return err
		}
		if _, err := writer.Write(data[:length]); err != nil {
			return err
		}
		data = data[length:]
	}
	return nil
}

func (s *Server) handleContainerStats(writer http.ResponseWriter, request *http.Request, query string) {
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status != "running" || record.PID <= 0 {
		writeError(writer, http.StatusConflict, fmt.Errorf("container is not running"))
		return
	}
	stream := true
	if raw := request.URL.Query().Get("stream"); raw != "" {
		stream, err = strconv.ParseBool(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("query parameter stream must be boolean"))
			return
		}
	}
	writer.Header().Set("Content-Type", "application/json")
	if !stream {
		payload, statsErr := dockerStatsSnapshot(record)
		if statsErr != nil {
			writeError(writer, http.StatusNotImplemented, statsErr)
			return
		}
		writeJSON(writer, http.StatusOK, payload)
		return
	}
	flusher, canFlush := writer.(http.Flusher)
	if !canFlush {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("streaming responses are unavailable"))
		return
	}
	writer.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(writer)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		payload, statsErr := dockerStatsSnapshot(record)
		if statsErr != nil {
			return
		}
		if encoder.Encode(payload) != nil {
			return
		}
		flusher.Flush()
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			current, getErr := state.Get(query)
			if getErr != nil || current.Status != "running" || current.PID <= 0 {
				return
			}
			record = current
		}
	}
}

func dockerStatsSnapshot(record state.Record) (map[string]any, error) {
	stats, err := state.ReadStats(record.PID)
	if err != nil {
		return nil, err
	}
	memoryLimit := int64(0)
	if value, parseErr := strconv.ParseInt(stats.MemoryMax, 10, 64); parseErr == nil && value > 0 {
		memoryLimit = value
	}
	cpuUsage := map[string]any{
		"total_usage": stats.CPUUsageUsec * 1000,
		"usage_in_kernelmode": stats.CPUSystemUsec * 1000,
		"usage_in_usermode": stats.CPUUserUsec * 1000,
	}
	return map[string]any{
		"read": time.Now().UTC().Format(time.RFC3339Nano),
		"preread": "",
		"pids_stats": map[string]any{"current": stats.PIDsCurrent, "limit": record.Options.PidsLimit},
		"memory_stats": map[string]any{"usage": stats.MemoryCurrent, "limit": memoryLimit, "stats": map[string]int64{}},
		"cpu_stats": map[string]any{
			"cpu_usage": cpuUsage, "system_cpu_usage": 0, "online_cpus": runtime.NumCPU(),
		},
		"precpu_stats": map[string]any{"cpu_usage": cpuUsage, "system_cpu_usage": 0, "online_cpus": runtime.NumCPU()},
		"blkio_stats": map[string]any{"io_service_bytes_recursive": []any{}},
		"networks": map[string]any{},
		"name": displayName(record), "id": record.ID,
	}, nil
}

func (s *Server) handleImages(writer http.ResponseWriter) {
	images, err := imageStore.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	containersByImage := make(map[string]int)
	for _, record := range records {
		if image := record.Labels["io.boxr.image"]; image != "" {
			containersByImage[image]++
		}
	}
	result := make([]map[string]any, 0, len(images))
	for _, image := range images {
		result = append(result, map[string]any{
			"Id": "sha256:" + image.Digest, "ParentId": "", "RepoTags": []string{image.Reference},
			"RepoDigests": []string{}, "Created": image.Created.Unix(), "Size": image.LayerBytes,
			"SharedSize": -1, "Containers": containersByImage[image.Reference], "Labels": map[string]string{},
			"Architecture": image.Arch, "Os": image.OS, "Variant": image.Variant,
		})
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) handleEvents(writer http.ResponseWriter, request *http.Request) {
	flusher, canFlush := writer.(http.Flusher)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	events, err := state.Events(0)
	if err != nil {
		return
	}
	since, _ := parseTimeQuery(request.URL.Query().Get("since"))
	until, _ := parseTimeQuery(request.URL.Query().Get("until"))
	offset := 0
	send := func() bool {
		if offset > len(events) {
			offset = 0
		}
		for offset < len(events) {
			event := events[offset]
			offset++
			if !since.IsZero() && event.Time.Before(since) {
				continue
			}
			if !until.IsZero() && event.Time.After(until) {
				return false
			}
			payload := map[string]any{
				"Type": "container", "Action": dockerEventAction(event.Action),
				"Actor": map[string]any{
					"ID": event.ID, "Attributes": map[string]string{"name": event.Name},
				},
				"time": event.Time.Unix(), "timeNano": event.Time.UnixNano(),
			}
			if json.NewEncoder(writer).Encode(payload) != nil {
				return false
			}
			if canFlush {
				flusher.Flush()
			}
		}
		return true
	}
	if !send() || !until.IsZero() {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			events, err = state.Events(0)
			if err != nil || !send() {
				return
			}
		}
	}
}

func dockerEventAction(value string) string {
	switch value {
	case "create":
		return "create"
	case "start":
		return "start"
	case "die":
		return "die"
	case "remove":
		return "destroy"
	case "restart":
		return "restart"
	case "stop-requested":
		return "stop"
	case "restart-cancelled":
		return "stop"
	default:
		return value
	}
}

func parseTimeQuery(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(number, 0), nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func boolQuery(request *http.Request, key string) (bool, error) {
	value := request.URL.Query().Get(key)
	if value == "" {
		return false, nil
	}
	result, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("query parameter %s must be boolean", key)
	}
	return result, nil
}

func intQuery(request *http.Request, key string) (int, error) {
	value := request.URL.Query().Get(key)
	if value == "" {
		return 0, nil
	}
	result, err := strconv.Atoi(value)
	if err != nil || result < 0 {
		return 0, fmt.Errorf("query parameter %s must be a non-negative integer", key)
	}
	return result, nil
}

func requireMethod(writer http.ResponseWriter, request *http.Request, method string) bool {
	if request.Method == method {
		return true
	}
	writeMethodError(writer)
	return false
}

func writeMethodError(writer http.ResponseWriter) {
	writer.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
	writeError(writer, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"message": err.Error()})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func write(writer http.ResponseWriter, status int, contentType string, data []byte) {
	writer.Header().Set("Content-Type", contentType)
	writer.WriteHeader(status)
	if len(data) > 0 {
		_, _ = writer.Write(data)
	}
}

