// Package engineapi exposes Boxr's local runtime state through a conservative
// Docker Engine HTTP API read surface. It deliberately does not claim that
// unsupported Docker mutations or runtime features are implemented.
package engineapi

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	boxrnetwork "github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

const (
	apiVersion    = "1.45"
	minAPIVersion = "1.24"
)

type Server struct {
	version               string
	containerTransitions  containerTransitionLocks
	imageMu               sync.Mutex
	volumeMu              sync.Mutex
	containerPruneMu      sync.Mutex
	networkMu             sync.Mutex
	execMu                sync.Mutex
	execSessions          map[string]*dockerExecSession
	createNetwork         func(string, boxrnetwork.CreateOptions) (boxrnetwork.Network, error)
	removeNetwork         func(string) error
	joinNetwork           func(string, int, string, boxrnetwork.EndpointOptions, ...boxrnetwork.PortMapping) (string, error)
	leaveNetwork          func(string, string) error
	verifyNetworkPID      func(state.Record) error
	verifyExecPID         func(state.Record) error
	verifyUpdatePID       func(state.Record) error
	updateCgroupResources func(int, container.Options, container.CgroupResourceUpdate) error
	setCgroupFrozen       func(int, bool) error
}

func New(version string) *Server {
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	return &Server{
		version:               version,
		createNetwork:         boxrnetwork.Create,
		removeNetwork:         boxrnetwork.Remove,
		joinNetwork:           boxrnetwork.JoinWithOptions,
		leaveNetwork:          boxrnetwork.Leave,
		verifyNetworkPID:      verifyDockerContainerPID,
		verifyExecPID:         verifyDockerContainerPID,
		verifyUpdatePID:       verifyDockerContainerPID,
		updateCgroupResources: container.UpdateCgroupResources,
		setCgroupFrozen:       container.SetCgroupFrozen,
		execSessions:          make(map[string]*dockerExecSession),
	}
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
	case path == "/commit":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleContainerCommit(writer, request)
	case path == "/build":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleImageBuild(writer, request)
	case path == "/info":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleInfo(writer)
	case path == "/containers/create":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleContainerCreate(writer, request)
	case path == "/containers/prune":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleContainerPrune(writer, request)
	case path == "/containers/json":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleContainers(writer, request)
	case strings.HasPrefix(path, "/containers/"):
		remainder := strings.TrimPrefix(path, "/containers/")
		if strings.HasSuffix(remainder, "/pause") {
			containerID := strings.TrimSuffix(remainder, "/pause")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container pause endpoint not found"))
				return
			}
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerPause(writer, request, containerID)
			return
		}
		if strings.HasSuffix(remainder, "/unpause") {
			containerID := strings.TrimSuffix(remainder, "/unpause")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container unpause endpoint not found"))
				return
			}
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerUnpause(writer, request, containerID)
			return
		}
		if strings.HasSuffix(remainder, "/export") {
			containerID := strings.TrimSuffix(remainder, "/export")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container export endpoint not found"))
				return
			}
			if !requireMethod(writer, request, http.MethodGet) {
				return
			}
			s.handleContainerExport(writer, request, containerID)
			return
		}
		if strings.HasSuffix(remainder, "/attach") {
			containerID := strings.TrimSuffix(remainder, "/attach")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container attach endpoint not found"))
				return
			}
			s.handleContainerAttach(writer, request, containerID)
			return
		}
		if strings.HasSuffix(remainder, "/archive") {
			containerID := strings.TrimSuffix(remainder, "/archive")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container archive endpoint not found"))
				return
			}
			switch request.Method {
			case http.MethodGet, http.MethodHead, http.MethodPut:
				s.handleContainerArchive(writer, request, containerID)
			default:
				writeMethodErrorFor(writer, http.MethodGet+", "+http.MethodHead+", "+http.MethodPut)
			}
			return
		}
		if strings.HasSuffix(remainder, "/changes") {
			containerID := strings.TrimSuffix(remainder, "/changes")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container changes endpoint not found"))
				return
			}
			if !requireMethod(writer, request, http.MethodGet) {
				return
			}
			s.handleContainerChanges(writer, request, containerID)
			return
		}
		if strings.HasSuffix(remainder, "/update") {
			containerID := strings.TrimSuffix(remainder, "/update")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container update endpoint not found"))
				return
			}
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerUpdate(writer, request, containerID)
			return
		}
		if strings.HasSuffix(remainder, "/exec") {
			containerID := strings.TrimSuffix(remainder, "/exec")
			if containerID == "" || strings.Contains(containerID, "/") {
				writeError(writer, http.StatusNotFound, fmt.Errorf("container exec endpoint not found"))
				return
			}
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerExecCreate(writer, request, containerID)
			return
		}
		if strings.HasSuffix(remainder, "/kill") {
			if !requireMethodAllow(writer, request, http.MethodPost, http.MethodPost) {
				return
			}
			s.handleContainerKill(writer, request, strings.TrimSuffix(remainder, "/kill"))
			return
		}
		if strings.HasSuffix(remainder, "/wait") {
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerWait(writer, request, strings.TrimSuffix(remainder, "/wait"))
			return
		}
		if strings.HasSuffix(remainder, "/rename") {
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerRename(writer, request, strings.TrimSuffix(remainder, "/rename"))
			return
		}
		if strings.HasSuffix(remainder, "/restart") {
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerRestart(writer, request, strings.TrimSuffix(remainder, "/restart"))
			return
		}
		if strings.HasSuffix(remainder, "/start") {
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerStart(writer, request, strings.TrimSuffix(remainder, "/start"))
			return
		}
		if strings.HasSuffix(remainder, "/stop") {
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleContainerStop(writer, request, strings.TrimSuffix(remainder, "/stop"))
			return
		}
		if !strings.Contains(remainder, "/") && request.Method == http.MethodDelete {
			s.handleContainerDelete(writer, request, remainder)
			return
		}
		if request.Method != http.MethodGet {
			writeMethodErrorFor(writer, http.MethodGet+", "+http.MethodHead)
			return
		}
		s.handleContainer(writer, request, remainder)
	case strings.HasPrefix(path, "/exec/"):
		remainder := strings.TrimPrefix(path, "/exec/")
		parts := strings.Split(remainder, "/")
		if len(parts) == 1 && parts[0] != "" && request.Method == http.MethodDelete {
			execID, err := url.PathUnescape(parts[0])
			if err != nil || strings.Contains(execID, "/") {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid exec identifier"))
				return
			}
			s.handleExecDelete(writer, execID)
			return
		}
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			writeError(writer, http.StatusNotFound, fmt.Errorf("exec endpoint not found"))
			return
		}
		execID, err := url.PathUnescape(parts[0])
		if err != nil || strings.Contains(execID, "/") {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid exec identifier"))
			return
		}
		switch parts[1] {
		case "start":
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleExecStart(writer, request, execID)
		case "json":
			if !requireMethod(writer, request, http.MethodGet) {
				return
			}
			s.handleExecInspect(writer, execID)
		case "resize":
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleExecResize(writer, request, execID)
		default:
			writeError(writer, http.StatusNotFound, fmt.Errorf("exec endpoint not found"))
		}
	case path == "/images/prune":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleImagePrune(writer, request)
	case path == "/images/load":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleImageLoad(writer, request)
	case path == "/images/get":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleImageExportMany(writer, request)
	case path == "/images/create":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleImageCreate(writer, request)
	case path == "/images/json":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleImages(writer)
	case strings.HasPrefix(path, "/images/"):
		remainder := strings.TrimPrefix(path, "/images/")
		if strings.HasSuffix(remainder, "/get") {
			if !requireMethod(writer, request, http.MethodGet) {
				return
			}
			s.handleImageExport(writer, request, strings.TrimSuffix(remainder, "/get"))
			return
		}
		if strings.HasSuffix(remainder, "/tag") {
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleImageTag(writer, request, strings.TrimSuffix(remainder, "/tag"))
			return
		}
		if strings.HasSuffix(remainder, "/push") {
			if !requireMethod(writer, request, http.MethodPost) {
				return
			}
			s.handleImagePush(writer, request, strings.TrimSuffix(remainder, "/push"))
			return
		}
		if strings.HasSuffix(remainder, "/history") {
			if !requireMethod(writer, request, http.MethodGet) {
				return
			}
			s.handleImageHistory(writer, strings.TrimSuffix(remainder, "/history"))
			return
		}
		if strings.HasSuffix(remainder, "/json") {
			if !requireMethod(writer, request, http.MethodGet) {
				return
			}
			s.handleImageInspect(writer, strings.TrimSuffix(remainder, "/json"))
			return
		}
		if request.Method == http.MethodDelete {
			s.handleImageDelete(writer, request, remainder)
			return
		}
		writeMethodErrorFor(writer, http.MethodDelete)
	case path == "/volumes/prune":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleVolumePrune(writer, request)
	case path == "/volumes":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleVolumes(writer, request)
	case path == "/volumes/create":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleVolumeCreate(writer, request)
	case strings.HasPrefix(path, "/volumes/"):
		remainder := strings.TrimPrefix(path, "/volumes/")
		if strings.Contains(remainder, "/") || remainder == "" {
			writeError(writer, http.StatusNotFound, fmt.Errorf("volume endpoint not found"))
			return
		}
		name, err := url.PathUnescape(remainder)
		if err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid volume name"))
			return
		}
		switch request.Method {
		case http.MethodGet:
			s.handleVolumeInspect(writer, name)
		case http.MethodDelete:
			s.handleVolumeDelete(writer, name)
		default:
			writeMethodErrorFor(writer, http.MethodGet+", "+http.MethodDelete)
		}
	case path == "/networks/prune":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleNetworkPrune(writer, request)
	case path == "/networks":
		if !requireMethod(writer, request, http.MethodGet) {
			return
		}
		s.handleNetworks(writer, request)
	case path == "/networks/create":
		if !requireMethod(writer, request, http.MethodPost) {
			return
		}
		s.handleNetworkCreate(writer, request)
	case strings.HasPrefix(path, "/networks/"):
		remainder := strings.TrimPrefix(path, "/networks/")
		parts := strings.Split(remainder, "/")
		if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
			writeError(writer, http.StatusNotFound, fmt.Errorf("network endpoint not found"))
			return
		}
		name, err := url.PathUnescape(parts[0])
		if err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid network identifier"))
			return
		}
		if len(parts) == 2 {
			switch parts[1] {
			case "connect":
				if !requireMethod(writer, request, http.MethodPost) {
					return
				}
				s.handleNetworkConnect(writer, request, name)
			case "disconnect":
				if !requireMethod(writer, request, http.MethodPost) {
					return
				}
				s.handleNetworkDisconnect(writer, request, name)
			default:
				writeError(writer, http.StatusNotFound, fmt.Errorf("network endpoint not found"))
			}
			return
		}
		switch request.Method {
		case http.MethodGet:
			s.handleNetworkInspect(writer, name)
		case http.MethodDelete:
			s.handleNetworkDelete(writer, name)
		default:
			writeMethodErrorFor(writer, http.MethodGet+", "+http.MethodDelete)
		}

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
	running, paused, stopped := 0, 0, 0
	for _, record := range records {
		switch record.Status {
		case "running", "restarting":
			running++
		case "paused":
			paused++
		default:
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
		"ContainersPaused": paused, "ContainersStopped": stopped, "Images": len(images),
		"Driver": "boxr-rootfs", "DriverStatus": [][]string{}, "BoxrStateDir": stateDir,
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

func dockerImageID(digest string) string {
	if strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return "sha256:" + digest
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

func dockerStatusWithHealth(status string, health *state.HealthState) string {
	if health == nil || (status != "Up" && !strings.HasPrefix(status, "Up ")) {
		return status
	}
	switch health.Status {
	case "healthy", "unhealthy":
		return status + " (" + health.Status + ")"
	case "starting":
		return status + " (health: starting)"
	default:
		return status
	}
}

func displayName(record state.Record) string {
	if record.Name != "" {
		return "/" + record.Name
	}
	return "/" + record.ID
}

func (s *Server) containerNetworkAttachments(records []state.Record) (map[string]map[string]any, error) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	return dockerNetworkAttachments(records)
}

func dockerNetworkAttachments(records []state.Record) (map[string]map[string]any, error) {
	summaries, err := boxrnetwork.List()
	if err != nil {
		return nil, fmt.Errorf("list managed container networks: %w", err)
	}
	networks := make([]boxrnetwork.Network, 0, len(summaries))
	for _, summary := range summaries {
		item, err := boxrnetwork.InspectDetails(summary.Name)
		if err != nil {
			return nil, fmt.Errorf("inspect managed network %q: %w", summary.Name, err)
		}
		networks = append(networks, item)
	}
	return dockerNetworkAttachmentsFromNetworks(networks, records)
}

// dockerNetworkAttachmentsFromNetworks maps persisted Boxr IPAM leases into
// Docker's per-container NetworkSettings.Networks representation. It reports
// corrupt persisted addresses instead of returning misleading network data.
func dockerNetworkAttachmentsFromNetworks(networks []boxrnetwork.Network, records []state.Record) (map[string]map[string]any, error) {
	knownContainers := make(map[string]struct{}, len(records))
	for _, record := range records {
		knownContainers[record.ID] = struct{}{}
	}
	result := make(map[string]map[string]any, len(records))
	for _, record := range records {
		result[record.ID] = make(map[string]any)
	}
	for _, network := range networks {
		if len(network.Allocations) == 0 {
			continue
		}
		prefix, err := netip.ParsePrefix(network.Subnet)
		if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() {
			return nil, fmt.Errorf("managed network %q has an invalid IPv4 subnet", network.Name)
		}
		gateway, err := netip.ParseAddr(network.Gateway)
		if err != nil || !gateway.Is4() || !prefix.Contains(gateway) {
			return nil, fmt.Errorf("managed network %q has an invalid IPv4 gateway", network.Name)
		}
		if strings.TrimSpace(network.Name) == "" || strings.TrimSpace(network.ID) == "" {
			return nil, fmt.Errorf("managed network has an empty name or ID")
		}
		for containerID, rawAddress := range network.Allocations {
			if _, exists := knownContainers[containerID]; !exists {
				continue
			}
			address, err := netip.ParseAddr(rawAddress)
			if err != nil || !address.Is4() || !prefix.Contains(address) || address == gateway {
				return nil, fmt.Errorf("managed network %q has an invalid IPv4 lease for container %s", network.Name, containerID)
			}
			attachments := result[containerID]
			if attachments == nil {
				attachments = make(map[string]any)
				result[containerID] = attachments
			}
			attachments[network.Name] = map[string]any{
				"IPAMConfig": nil, "Links": nil, "Aliases": []string{},
				"NetworkID": network.ID, "EndpointID": network.EndpointIDs[containerID],
				"Gateway": gateway.String(), "IPAddress": address.String(),
				"IPPrefixLen": prefix.Bits(), "IPv6Gateway": "",
				"GlobalIPv6Address": "", "GlobalIPv6PrefixLen": 0,
				"MacAddress": network.MACAddresses[containerID], "DriverOpts": nil,
			}
		}
	}
	return result, nil
}

func dockerContainerNetworkSettings(record state.Record, ports map[string][]map[string]string, attachments map[string]map[string]any) map[string]any {
	networks := attachments[record.ID]
	if networks == nil {
		networks = make(map[string]any)
	}
	settings := map[string]any{
		"Ports": ports, "Networks": networks,
		"Bridge": "", "EndpointID": "", "Gateway": "",
		"GlobalIPv6Address": "", "GlobalIPv6PrefixLen": 0,
		"IPAddress": "", "IPPrefixLen": 0, "IPv6Gateway": "",
		"MacAddress": "",
	}
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	primary := record.Options.NetworkName
	if _, exists := networks[primary]; !exists && len(names) > 0 {
		primary = names[0]
	}
	if value, exists := networks[primary]; exists {
		if endpoint, ok := value.(map[string]any); ok {
			settings["Gateway"] = endpoint["Gateway"]
			settings["IPAddress"] = endpoint["IPAddress"]
			settings["IPPrefixLen"] = endpoint["IPPrefixLen"]
			settings["IPv6Gateway"] = endpoint["IPv6Gateway"]
			settings["GlobalIPv6Address"] = endpoint["GlobalIPv6Address"]
			settings["GlobalIPv6PrefixLen"] = endpoint["GlobalIPv6PrefixLen"]
		}
	}
	return settings
}

const maxDockerSizeWalkEntries = 1 << 20

func dockerDirectoryLogicalSize(root string, missingIsZero bool) (int64, error) {
	if root == "" {
		return 0, nil
	}
	info, err := os.Lstat(root)
	if err != nil {
		if missingIsZero && errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("inspect filesystem size root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, fmt.Errorf("filesystem size root %q must be a real directory", root)
	}
	var total int64
	var entries int
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxDockerSizeWalkEntries {
			return fmt.Errorf("filesystem size walk exceeded %d entries", maxDockerSizeWalkEntries)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		size := fileInfo.Size()
		if size < 0 || total > int64(^uint64(0)>>1)-size {
			return fmt.Errorf("filesystem size overflow while inspecting %q", path)
		}
		total += size
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure filesystem tree %q: %w", root, err)
	}
	return total, nil
}

// dockerContainerSize reports logical bytes in the immutable base rootfs and
// Boxr's OverlayFS upper layer. Symlinks and special files are never followed.
func dockerContainerSize(record state.Record) (sizeRW, sizeRootFS int64, err error) {
	if record.Options.RootFS != "" {
		sizeRootFS, err = dockerDirectoryLogicalSize(record.Options.RootFS, false)
		if err != nil {
			return 0, 0, err
		}
	}
	if record.Options.OverlayDir != "" {
		sizeRW, err = dockerDirectoryLogicalSize(filepath.Join(record.Options.OverlayDir, "upper"), true)
		if err != nil {
			return 0, 0, err
		}
	}
	if sizeRootFS > int64(^uint64(0)>>1)-sizeRW {
		return 0, 0, fmt.Errorf("container filesystem size overflow")
	}
	return sizeRW, sizeRootFS + sizeRW, nil
}

func (s *Server) handleContainers(writer http.ResponseWriter, request *http.Request) {
	all, err := boolQuery(request, "all")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	includeSize, err := boolQuery(request, "size")
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
	filterTimes, err := resolveDockerContainerFilterTimes(filters)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	networkAttachments, err := s.containerNetworkAttachments(records)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		if !all && record.Status != "running" && record.Status != "paused" && record.Status != "restarting" {
			continue
		}
		if !matchesContainerFiltersWithTimes(record, filters, networkAttachments[record.ID], filterTimes) {
			continue
		}
		image := record.Labels["io.boxr.image"]
		imageID := ""
		if image != "" {
			if inspected, inspectErr := imageStore.Inspect(image); inspectErr == nil {
				imageID = dockerImageID(inspected.Digest)
			}
		}
		command := strings.Join(record.Command, " ")
		item := map[string]any{
			"Id": record.ID, "Names": []string{displayName(record)}, "Image": image,
			"ImageID": imageID, "Command": command, "Created": record.CreatedAt.Unix(),
			"Ports": portsForOptions(record.Options), "Labels": record.Labels,
			"State": containerState(record), "Status": dockerStatusWithHealth(containerStatus(record), record.Health),
			"HostConfig":      map[string]any{"NetworkMode": dockerNetworkMode(record.Options)},
			"NetworkSettings": map[string]any{"Networks": networkAttachments[record.ID]},
		}
		if includeSize {
			sizeRW, sizeRootFS, sizeErr := dockerContainerSize(record)
			if sizeErr != nil {
				writeError(writer, http.StatusInternalServerError, fmt.Errorf("measure container %s filesystem: %w", record.ID, sizeErr))
				return
			}
			item["SizeRw"] = sizeRW
			item["SizeRootFs"] = sizeRootFS
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

func dockerMemorySwapTotal(options container.Options) int64 {
	if options.MemorySwapUnlimited {
		return -1
	}
	if options.MemorySwapMaxSet || options.MemorySwapMaxBytes > 0 {
		const maxInt64 = int64(1<<63 - 1)
		if options.MemorySwapMaxBytes > maxInt64-options.MemoryLimitBytes {
			return maxInt64
		}
		return options.MemoryLimitBytes + options.MemorySwapMaxBytes
	}
	if options.MemoryLimitBytes > 0 {
		const maxInt64 = int64(1<<63 - 1)
		if options.MemoryLimitBytes > maxInt64/2 {
			return maxInt64
		}
		return options.MemoryLimitBytes * 2
	}
	return 0
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

var dockerContainerFilterKeys = map[string]struct{}{
	"ancestor": {}, "before": {}, "exited": {}, "expose": {}, "health": {},
	"id": {}, "label": {}, "label!": {}, "name": {}, "network": {},
	"publish": {}, "since": {}, "status": {}, "volume": {},
}

type dockerContainerFilterTimes struct {
	before []time.Time
	since  []time.Time
}

func resolveDockerContainerFilterTimes(filters map[string][]string) (dockerContainerFilterTimes, error) {
	var result dockerContainerFilterTimes
	for key, values := range filters {
		if _, supported := dockerContainerFilterKeys[key]; !supported {
			return result, fmt.Errorf("unsupported container filter %q", key)
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
				return result, fmt.Errorf("container filter %q contains an empty or whitespace-padded value", key)
			}
			if key != "before" && key != "since" {
				continue
			}
			reference, err := state.Get(value)
			if err != nil {
				return result, fmt.Errorf("resolve %s container %q: %w", key, value, err)
			}
			if key == "before" {
				result.before = append(result.before, reference.CreatedAt)
			} else {
				result.since = append(result.since, reference.CreatedAt)
			}
		}
	}
	return result, nil
}

func matchesContainerFilters(record state.Record, filters map[string][]string, networkAttachments ...map[string]any) bool {
	var attachedNetworks map[string]any
	if len(networkAttachments) > 0 {
		attachedNetworks = networkAttachments[0]
	}
	return matchesContainerFiltersWithTimes(record, filters, attachedNetworks, dockerContainerFilterTimes{})
}

func matchesContainerFiltersWithTimes(record state.Record, filters map[string][]string, attachedNetworks map[string]any, referenceTimes dockerContainerFilterTimes) bool {
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
		case "before":
			matched := false
			for _, createdAt := range referenceTimes.before {
				if record.CreatedAt.Before(createdAt) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		case "since":
			matched := false
			for _, createdAt := range referenceTimes.since {
				if !record.CreatedAt.Before(createdAt) {
					matched = true
					break
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
		case "label!":
			for _, value := range values {
				key, excluded, hasValue := strings.Cut(value, "=")
				actual, exists := record.Labels[key]
				if exists && (!hasValue || actual == excluded) {
					return false
				}
			}
		case "exited":
			if record.ExitCode == nil {
				return false
			}
			matched := false
			for _, value := range values {
				if value == strconv.Itoa(*record.ExitCode) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		case "health":
			actual := "none"
			if record.Health != nil && record.Health.Status != "" {
				actual = record.Health.Status
			}
			matched := false
			for _, value := range values {
				if value == actual {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		case "ancestor":
			imageReference := record.Labels["io.boxr.image"]
			matched := false
			for _, value := range values {
				if imageReference == value || (imageReference != "" && strings.HasPrefix(imageReference, value)) {
					matched = true
					break
				}
				if imageReference != "" {
					image, err := imageStore.Inspect(imageReference)
					if err == nil {
						imageID := dockerImageID(image.Digest)
						if strings.HasPrefix(imageID, value) || strings.TrimPrefix(imageID, "sha256:") == value {
							matched = true
							break
						}
					}
				}
			}
			if !matched {
				return false
			}
		case "volume":
			matched := false
			for _, value := range values {
				for _, mount := range record.Options.BindMounts {
					if mount.Target == value || mount.Source == value || mount.VolumeName == value {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				return false
			}
		case "publish":
			matched := false
			for _, value := range values {
				for _, mapping := range record.Options.PublishedPorts {
					protocol := mapping.Protocol
					if protocol == "" {
						protocol = "tcp"
					}
					if dockerPortFilterMatches(value, mapping.ContainerPort, protocol) ||
						dockerPortFilterMatches(value, mapping.HostPort, protocol) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				return false
			}
		case "expose":
			exposed, err := container.NormalizeExposedPorts(record.Options.ExposedPorts)
			if err != nil {
				return false
			}
			matched := false
			for _, value := range values {
				for _, exposedPort := range exposed {
					portText, protocol, _ := strings.Cut(exposedPort, "/")
					portNumber, err := strconv.ParseUint(portText, 10, 16)
					if err == nil && dockerPortFilterMatches(value, uint16(portNumber), protocol) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				return false
			}
		case "network":
			matched := false
			for _, value := range values {
				for name, rawEndpoint := range attachedNetworks {
					if name == value {
						matched = true
						break
					}
					endpoint, ok := rawEndpoint.(map[string]any)
					if !ok {
						continue
					}
					networkID, _ := endpoint["NetworkID"].(string)
					if networkID != "" && strings.HasPrefix(networkID, value) {
						matched = true
						break
					}
				}
				if matched {
					break
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

// dockerPortFilterMatches implements the Docker port filter syntax for a
// single candidate port. Ranges are bounded to avoid pathological inputs.
func dockerPortFilterMatches(filter string, candidate uint16, protocol string) bool {
	if candidate == 0 || filter == "" || strings.TrimSpace(filter) != filter {
		return false
	}
	portRange, wantedProtocol, hasProtocol := strings.Cut(filter, "/")
	if !hasProtocol {
		wantedProtocol = "tcp"
	}
	wantedProtocol = strings.ToLower(wantedProtocol)
	if wantedProtocol != "tcp" && wantedProtocol != "udp" && wantedProtocol != "sctp" {
		return false
	}
	protocol = strings.ToLower(protocol)
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != wantedProtocol {
		return false
	}
	firstText, lastText, hasRange := strings.Cut(portRange, "-")
	if !hasRange {
		lastText = firstText
	}
	first, firstErr := strconv.ParseUint(firstText, 10, 16)
	last, lastErr := strconv.ParseUint(lastText, 10, 16)
	if firstErr != nil || lastErr != nil || first == 0 || last < first || last-first+1 > 256 {
		return false
	}
	return uint64(candidate) >= first && uint64(candidate) <= last
}

func (s *Server) handleContainerKill(writer http.ResponseWriter, request *http.Request, rawID string) {
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	signal, err := dockerSignalName(request.URL.Query().Get("signal"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	unlock := s.containerTransitions.lock(query)
	defer unlock()
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status == "paused" {
		if err := s.resumePausedContainer(record); err != nil {
			writeError(writer, http.StatusConflict, fmt.Errorf("resume paused container before kill: %w", err))
			return
		}
		record, err = state.Get(record.ID)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("refresh resumed container: %w", err))
			return
		}
	}
	if record.Status != "running" {
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s is %s, not running", record.ID, record.Status))
		return
	}
	if err := state.Signal(record.ID, signal); err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerWait(writer http.ResponseWriter, request *http.Request, rawID string) {
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	condition, err := singleQueryValue(request.URL.Query(), "condition")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if condition == "" {
		condition = "not-running"
	}
	if condition != "not-running" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("unsupported wait condition %q; only not-running is supported", condition))
		return
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := state.Get(query)
		if err != nil {
			writeError(writer, http.StatusNotFound, err)
			return
		}
		if record.Status != "running" && record.Status != "paused" && record.Status != "restarting" {
			statusCode := 0
			if record.ExitCode != nil {
				statusCode = *record.ExitCode
			}
			writeJSON(writer, http.StatusOK, map[string]any{
				"StatusCode": statusCode,
				"Error":      nil,
			})
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) handleContainerRename(writer http.ResponseWriter, request *http.Request, rawID string) {
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	newName, err := singleQueryValue(request.URL.Query(), "name")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(newName) == "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("query parameter name is required"))
		return
	}
	if _, err := state.Rename(query, newName); err != nil {
		switch {
		case strings.Contains(err.Error(), "no container matches"):
			writeError(writer, http.StatusNotFound, err)
		case strings.Contains(err.Error(), "already in use"):
			writeError(writer, http.StatusConflict, err)
		default:
			writeError(writer, http.StatusBadRequest, err)
		}
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerDelete(writer http.ResponseWriter, request *http.Request, rawID string) {
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	force, err := boolQuery(request, "force")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	removeVolumes, err := boolQuery(request, "v")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if removeVolumes {
		s.volumeMu.Lock()
		defer s.volumeMu.Unlock()
	}
	unlock := s.containerTransitions.lock(query)
	defer unlock()
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status == "running" || record.Status == "paused" || record.Status == "restarting" {
		if !force {
			writeError(writer, http.StatusConflict, fmt.Errorf("container %s is %s; stop it before removal or set force=true", record.ID, record.Status))
			return
		}
		executable, err := os.Executable()
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err)
			return
		}
		stdout, stderr, launchErr := launchEngineAPICreatedContainer(executable, []string{"rm", "--force", record.ID})
		if launchErr != nil {
			message := strings.TrimSpace(stderr)
			if message == "" {
				message = strings.TrimSpace(stdout)
			}
			if message == "" {
				message = launchErr.Error()
			}
			writeError(writer, http.StatusConflict, fmt.Errorf("force remove container: %s", message))
			return
		}
		if _, verifyErr := state.Get(record.ID); verifyErr == nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("forced removal command returned without removing container %s", record.ID))
			return
		} else if !strings.Contains(verifyErr.Error(), "no container matches") {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("verify forced container removal: %w", verifyErr))
			return
		}
		if removeVolumes {
			if err := removeAnonymousVolumesForContainer(record); err != nil {
				writeError(writer, http.StatusInternalServerError, fmt.Errorf("remove unused anonymous container volumes: %w", err))
				return
			}
		}
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if record.Status == "created" {
		err = state.RemoveCreated(record.ID)
	} else {
		err = state.Remove(record.ID)
	}
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if removeVolumes {
		if err := removeAnonymousVolumesForContainer(record); err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("remove unused anonymous container volumes: %w", err))
			return
		}
	}
	writer.WriteHeader(http.StatusNoContent)
}

// removeAnonymousVolumesForContainer removes only anonymous volumes attached to
// this container, and only after the final persisted container reference is gone.
// Named volumes and volumes shared with another container are always retained.
func removeAnonymousVolumesForContainer(record state.Record) error {
	candidates := make(map[string]string)
	for _, mount := range record.Options.BindMounts {
		if mount.VolumeName == "" {
			continue
		}
		item, err := volume.Get(mount.VolumeName)
		if err != nil {
			return fmt.Errorf("inspect volume %q: %w", mount.VolumeName, err)
		}
		if item.Anonymous && item.Path == mount.Source {
			candidates[item.Name] = item.Path
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	records, err := state.List(true)
	if err != nil {
		return fmt.Errorf("inspect remaining volume users: %w", err)
	}
	var cleanupErr error
	for name, path := range candidates {
		inUse := false
		for _, candidate := range records {
			if candidate.ID == record.ID {
				continue
			}
			for _, mount := range candidate.Options.BindMounts {
				if mount.VolumeName == name || mount.Source == path {
					inUse = true
					break
				}
			}
			if inUse {
				break
			}
		}
		if inUse {
			continue
		}
		if err := volume.Remove(name); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove anonymous volume %q: %w", name, err))
		}
	}
	return cleanupErr
}

func decodeContainerID(rawID string) (string, error) {
	if rawID == "" || strings.Contains(rawID, "/") {
		return "", fmt.Errorf("container ID is required")
	}
	query, err := url.PathUnescape(rawID)
	if err != nil || strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("invalid container ID")
	}
	return query, nil
}

func dockerSignalName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "KILL", nil
	}
	if number, err := strconv.Atoi(value); err == nil {
		signals := map[int]string{
			1: "HUP", 2: "INT", 3: "QUIT", 9: "KILL", 10: "USR1",
			12: "USR2", 15: "TERM", 18: "CONT", 19: "STOP",
		}
		if name, ok := signals[number]; ok {
			return name, nil
		}
		return "", fmt.Errorf("signal %q is not supported", value)
	}
	name := strings.ToUpper(strings.TrimPrefix(value, "SIG"))
	switch name {
	case "HUP", "INT", "QUIT", "KILL", "USR1", "USR2", "TERM", "CONT", "STOP":
		return name, nil
	default:
		return "", fmt.Errorf("signal %q is not supported", value)
	}
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
	case "top":
		if request.Method != http.MethodGet {
			writeMethodErrorFor(writer, http.MethodGet)
			return
		}
		s.handleContainerTop(writer, request, query)
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
	volumeNames := make(map[string]string)
	storedVolumes, volumeErr := volume.List()
	if volumeErr != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect volume metadata: %w", volumeErr))
		return
	}
	for _, item := range storedVolumes {
		volumeNames[item.Path] = item.Name
	}
	mounts := make([]map[string]any, 0, len(record.Options.BindMounts)+len(record.Options.TmpfsMounts))
	for _, mount := range record.Options.BindMounts {
		mountType := "bind"
		mountInfo := map[string]any{
			"Type": mountType, "Source": mount.Source, "Destination": mount.Target,
			"Mode": dockerMountMode(mount.ReadOnly), "Propagation": "rprivate", "RW": !mount.ReadOnly,
		}
		if name, ok := volumeNames[mount.Source]; ok {
			mountInfo["Type"] = "volume"
			mountInfo["Name"] = name
			mountInfo["Driver"] = "local"
			delete(mountInfo, "Propagation")
		}
		mounts = append(mounts, mountInfo)
	}
	tmpfsHostConfig := make(map[string]string, len(record.Options.TmpfsMounts))
	for _, mount := range record.Options.TmpfsMounts {
		options := fmt.Sprintf("mode=%o", mount.Mode)
		if mount.SizeBytes > 0 {
			options = fmt.Sprintf("size=%d,%s", mount.SizeBytes, options)
		}
		tmpfsHostConfig[mount.Target] = options
		mounts = append(mounts, map[string]any{
			"Type": "tmpfs", "Source": "tmpfs", "Destination": mount.Target,
			"Mode": options, "RW": true,
		})
	}
	stateObject := map[string]any{
		"Status": containerState(record), "Running": running, "Paused": record.Status == "paused",
		"Restarting": record.Status == "restarting", "OOMKilled": false, "Dead": record.Status == "dead",
		"Pid": record.PID, "ExitCode": exitCode(record.ExitCode), "Error": record.Error,
		"StartedAt": startedAt, "FinishedAt": finishedAt,
	}
	if record.Health != nil {
		stateObject["Health"] = record.Health
	}
	configObject := map[string]any{
		"Hostname": record.Options.Hostname, "User": record.Options.User, "Cmd": record.Command,
		"Image": image, "WorkingDir": record.Options.Workdir, "Labels": record.Labels,
		"ExposedPorts": dockerContainerExposedPorts(record.Options.ExposedPorts),
		"StopSignal":   record.Options.StopSignal,
		"Healthcheck":  record.Options.Healthcheck,
		"OpenStdin":    false, "StdinOnce": false, "Tty": false,
	}
	hostConfig := map[string]any{
		"NetworkMode":       dockerNetworkMode(record.Options),
		"Memory":            record.Options.MemoryLimitBytes,
		"MemoryReservation": record.Options.MemoryLowBytes,
		"MemorySwap":        dockerMemorySwapTotal(record.Options),
		"NanoCpus":          int64(record.Options.CPUs * 1e9), "PidsLimit": record.Options.PidsLimit,
		"RestartPolicy": map[string]any{"Name": dockerRestartPolicy(record.Options.RestartPolicy), "MaximumRetryCount": record.Options.RestartMaxRetries},
		"PortBindings":  dockerPortBindings(record.Options), "Binds": dockerBinds(record.Options),
		"Tmpfs":      tmpfsHostConfig,
		"Privileged": false, "ReadonlyRootfs": record.Options.ReadOnlyRootFS,
	}
	networkAttachments, err := s.containerNetworkAttachments([]state.Record{record})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	networkSettings := dockerContainerNetworkSettings(record, ports, networkAttachments)
	writeJSON(writer, http.StatusOK, map[string]any{
		"Id": record.ID, "Created": created, "Path": path, "Args": commandArgs,
		"State": stateObject, "Image": image, "Name": displayName(record),
		"RestartCount": record.RestartCount, "Driver": "boxr-rootfs", "Platform": "linux",
		"Mounts": mounts, "HostConfig": hostConfig,
		"Config": configObject, "NetworkSettings": networkSettings,
	})
}

func dockerMountMode(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
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

func dockerContainerExposedPorts(values []string) map[string]struct{} {
	normalized, err := container.NormalizeExposedPorts(values)
	if err != nil {
		return map[string]struct{}{}
	}
	result := make(map[string]struct{}, len(normalized))
	for _, value := range normalized {
		result[value] = struct{}{}
	}
	return result
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
	volumes, _ := volume.List()
	volumeNames := make(map[string]string, len(volumes))
	for _, item := range volumes {
		volumeNames[item.Path] = item.Name
	}
	result := make([]string, 0, len(options.BindMounts))
	for _, mount := range options.BindMounts {
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		source := mount.Source
		if name, ok := volumeNames[mount.Source]; ok {
			source = name
		}
		result = append(result, source+":"+mount.Target+":"+mode)
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
	follow, err := boolQuery(request, "follow")
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
	if !follow {
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
		if err := writeDockerExecBytes(writer, header[:]); err != nil {
			return err
		}
		if err := writeDockerExecBytes(writer, data[:length]); err != nil {
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
	if err := state.VerifyProcessIdentity(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("container process identity is invalid: %w", err))
		return
	}
	if s.verifyExecPID == nil {
		writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("container process verification is unavailable"))
		return
	}
	if err := s.verifyExecPID(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("container process is not a verified Boxr workload: %w", err))
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
		payload, _, statsErr := dockerStatsSnapshotWithPrevious(record, nil)
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
	var previous *dockerStatsSample
	for {
		payload, currentSample, statsErr := dockerStatsSnapshotWithPrevious(record, previous)
		if statsErr != nil {
			return
		}
		if encoder.Encode(payload) != nil {
			return
		}
		flusher.Flush()
		previous = &currentSample
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			current, getErr := state.Get(query)
			if getErr != nil || current.Status != "running" || current.PID <= 0 {
				return
			}
			if current.PID != record.PID || current.ProcessStartTicks != record.ProcessStartTicks {
				previous = nil
			}
			if state.VerifyProcessIdentity(current) != nil || s.verifyExecPID(current) != nil {
				return
			}
			record = current
		}
	}
}

type dockerStatsSample struct {
	stats state.Stats
	read  time.Time
}

func dockerStatsSnapshot(record state.Record) (map[string]any, error) {
	payload, _, err := dockerStatsSnapshotWithPrevious(record, nil)
	return payload, err
}

func dockerStatsSnapshotWithPrevious(record state.Record, previous *dockerStatsSample) (map[string]any, dockerStatsSample, error) {
	stats, err := state.ReadStats(record.PID)
	if err != nil {
		return nil, dockerStatsSample{}, err
	}
	return dockerStatsSnapshotFromStats(record, stats, previous)
}

// dockerStatsSnapshotFromStats converts one cgroup snapshot into Docker's stats
// schema. Keeping conversion separate from host I/O makes counter handling
// deterministic and lets the compatibility schema be tested with fixtures.
func dockerStatsSnapshotFromStats(record state.Record, stats state.Stats, previous *dockerStatsSample) (map[string]any, dockerStatsSample, error) {
	now := time.Now().UTC()
	memoryLimit := stats.SystemMemoryTotalBytes
	if value, parseErr := strconv.ParseInt(stats.MemoryMax, 10, 64); parseErr == nil && value > 0 {
		memoryLimit = value
	}
	if memoryLimit < 0 {
		memoryLimit = 0
	}
	pidsLimit := stats.PIDsMax
	if pidsLimit <= 0 && record.Options.PidsLimit > 0 {
		pidsLimit = record.Options.PidsLimit
	}

	cpuUsage := dockerStatsCPUUsage(stats)
	preCPUUsage := map[string]any{
		"total_usage": int64(0), "usage_in_kernelmode": int64(0), "usage_in_usermode": int64(0),
		"percpu_usage": []int64{},
	}
	preSystemCPUUsage := int64(0)
	preread := ""
	if previous != nil {
		preCPUUsage = dockerStatsCPUUsage(previous.stats)
		preSystemCPUUsage = previous.stats.SystemCPUUsageNS
		preread = previous.read.Format(time.RFC3339Nano)
	}
	memoryStats := make(map[string]int64, len(stats.MemoryStats))
	for key, value := range stats.MemoryStats {
		memoryStats[key] = value
	}
	if memoryStats == nil {
		memoryStats = make(map[string]int64)
	}
	maxUsage := memoryStats["max_usage"]
	networks := make(map[string]any, len(stats.Networks))
	for name, counters := range stats.Networks {
		networks[name] = map[string]any{
			"rx_bytes": counters.RxBytes, "rx_packets": counters.RxPackets,
			"rx_errors": counters.RxErrors, "rx_dropped": counters.RxDropped,
			"tx_bytes": counters.TxBytes, "tx_packets": counters.TxPackets,
			"tx_errors": counters.TxErrors, "tx_dropped": counters.TxDropped,
		}
	}
	payload := map[string]any{
		"read":    now.Format(time.RFC3339Nano),
		"preread": preread,
		"pids_stats": map[string]any{
			"current": stats.PIDsCurrent, "limit": pidsLimit,
		},
		"memory_stats": map[string]any{
			"usage": stats.MemoryCurrent, "max_usage": maxUsage, "limit": memoryLimit,
			"stats": memoryStats,
		},
		"cpu_stats": map[string]any{
			"cpu_usage": cpuUsage, "system_cpu_usage": stats.SystemCPUUsageNS,
			"online_cpus": runtime.NumCPU(),
			"throttling_data": map[string]any{
				"periods": stats.CPUPeriods, "throttled_periods": stats.CPUThrottledPeriods,
				"throttled_time": stats.CPUThrottledUsec * 1000,
			},
		},
		"precpu_stats": map[string]any{
			"cpu_usage": preCPUUsage, "system_cpu_usage": preSystemCPUUsage,
			"online_cpus": runtime.NumCPU(),
		},
		"blkio_stats": dockerStatsBlockIO(stats.IOCounters),
		"networks":    networks,
		"name":        displayName(record), "id": record.ID,
	}
	return payload, dockerStatsSample{stats: stats, read: now}, nil
}

func dockerStatsCPUUsage(stats state.Stats) map[string]any {
	return map[string]any{
		"total_usage":         stats.CPUUsageUsec * 1000,
		"usage_in_kernelmode": stats.CPUSystemUsec * 1000,
		"usage_in_usermode":   stats.CPUUserUsec * 1000,
		"percpu_usage":        []int64{},
	}
}

func dockerStatsBlockIO(counters map[string]string) map[string]any {
	serviceBytes := make([]map[string]any, 0, len(counters)*2)
	serviced := make([]map[string]any, 0, len(counters)*2)
	for device, raw := range counters {
		majorText, minorText, ok := strings.Cut(device, ":")
		if !ok {
			continue
		}
		major, majorErr := strconv.ParseInt(majorText, 10, 64)
		minor, minorErr := strconv.ParseInt(minorText, 10, 64)
		if majorErr != nil || minorErr != nil {
			continue
		}
		values := make(map[string]int64)
		for _, field := range strings.Fields(raw) {
			key, valueText, found := strings.Cut(field, "=")
			if !found {
				continue
			}
			value, parseErr := strconv.ParseInt(valueText, 10, 64)
			if parseErr == nil {
				values[key] = value
			}
		}
		for _, operation := range []struct {
			name  string
			bytes string
			ios   string
		}{{name: "Read", bytes: "rbytes", ios: "rios"}, {name: "Write", bytes: "wbytes", ios: "wios"}} {
			serviceBytes = append(serviceBytes, map[string]any{
				"major": major, "minor": minor, "op": operation.name, "value": values[operation.bytes],
			})
			serviced = append(serviced, map[string]any{
				"major": major, "minor": minor, "op": operation.name, "value": values[operation.ios],
			})
		}
	}
	return map[string]any{
		"io_service_bytes_recursive": serviceBytes,
		"io_serviced_recursive":      serviced,
		"io_service_time_recursive":  []any{},
		"io_wait_time_recursive":     []any{},
		"io_merged_recursive":        []any{},
		"io_time_recursive":          []any{},
		"sectors_recursive":          []any{},
	}
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
			"Id": dockerImageID(image.Digest), "ParentId": "", "RepoTags": []string{image.Reference},
			"RepoDigests": []string{}, "Created": image.Created.Unix(), "Size": image.LayerBytes,
			"SharedSize": -1, "Containers": containersByImage[image.Reference], "Labels": map[string]string{},
			"Architecture": image.Arch, "Os": image.OS, "Variant": image.Variant,
		})
	}
	writeJSON(writer, http.StatusOK, result)
}

func decodeDockerImageReference(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return "", fmt.Errorf("image reference is required")
	}
	reference, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("invalid image reference encoding")
	}
	normalized, err := normalizeDockerLocalImageReference(reference)
	if err != nil {
		return "", err
	}
	return normalized, nil
}

func dockerImageConfig(config imageStore.ImageRuntimeConfig) map[string]any {
	env := append([]string{}, config.Env...)
	command := append([]string{}, config.Cmd...)
	entrypoint := append([]string{}, config.Entrypoint...)
	labels := make(map[string]string, len(config.Labels))
	for key, value := range config.Labels {
		labels[key] = value
	}
	volumes := make(map[string]struct{}, len(config.Volumes))
	for target := range config.Volumes {
		volumes[target] = struct{}{}
	}
	exposedPorts := make(map[string]struct{}, len(config.ExposedPorts))
	for target := range config.ExposedPorts {
		exposedPorts[target] = struct{}{}
	}
	return map[string]any{
		"Env": env, "Cmd": command, "Entrypoint": entrypoint,
		"WorkingDir": config.WorkingDir, "User": config.User, "Labels": labels,
		"Volumes": volumes, "ExposedPorts": exposedPorts, "StopSignal": config.StopSignal,
		"Healthcheck": config.Healthcheck,
	}
}

func (s *Server) handleImageInspect(writer http.ResponseWriter, rawReference string) {
	reference, err := decodeDockerImageReference(rawReference)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	image, err := imageStore.Inspect(reference)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusInternalServerError, err)
		}
		return
	}
	config := dockerImageConfig(image.RuntimeConfig)
	created := image.Created.UTC().Format(time.RFC3339Nano)
	writeJSON(writer, http.StatusOK, map[string]any{
		"Id": dockerImageID(image.Digest), "RepoTags": []string{image.Reference},
		"RepoDigests": []string{}, "Parent": "", "Comment": "",
		"Created": created, "Container": "", "DockerVersion": "", "Author": "",
		"Architecture": image.Arch, "Os": image.OS, "Variant": image.Variant,
		"Size": image.LayerBytes, "VirtualSize": image.LayerBytes,
		"Config": config, "ContainerConfig": config,
		"GraphDriver": map[string]any{"Name": "boxr-oci", "Data": map[string]string{}},
		"RootFS":      map[string]any{"Type": "layers", "Layers": append([]string{}, image.DiffIDs...)},
		"Metadata":    map[string]string{"LastTagTime": created},
	})
}

func (s *Server) handleImageDelete(writer http.ResponseWriter, request *http.Request, rawReference string) {
	reference, err := decodeDockerImageReference(rawReference)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	force, err := boolQuery(request, "force")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if _, err := boolQuery(request, "noprune"); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}

	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	images, err := imageStore.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	found := false
	for _, image := range images {
		if image.Reference == reference {
			found = true
			break
		}
	}
	if !found {
		writeError(writer, http.StatusNotFound, fmt.Errorf("image %q not found", reference))
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	for _, record := range records {
		if record.Labels["io.boxr.image"] == reference {
			message := fmt.Sprintf("image %q is referenced by container %s", reference, record.ID)
			if force {
				message += "; forced removal cannot orphan a container that depends on this image reference"
			}
			writeError(writer, http.StatusConflict, fmt.Errorf("%s", message))
			return
		}
	}
	if err := imageStore.Remove(reference); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(writer, http.StatusOK, []map[string]string{{"Untagged": reference}})
}

func (s *Server) handleEvents(writer http.ResponseWriter, request *http.Request) {
	since, err := parseTimeQuery(request.URL.Query().Get("since"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid since timestamp: %w", err))
		return
	}
	until, err := parseTimeQuery(request.URL.Query().Get("until"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid until timestamp: %w", err))
		return
	}
	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	events, err := state.Events(0)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	flusher, canFlush := writer.(http.Flusher)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
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
			if !matchesEventFilters(event, filters) {
				continue
			}
			attributes := make(map[string]string, 2)
			if event.Action == "prune-completed" {
				reclaimed := int64(0)
				if event.Reclaimed != nil {
					reclaimed = *event.Reclaimed
				}
				attributes["reclaimed"] = strconv.FormatInt(reclaimed, 10)
			} else {
				attributes["name"] = event.Name
			}
			action := dockerEventAction(event.Action)
			payload := map[string]any{
				"Type": "container", "Action": action,
				"Actor": map[string]any{
					"ID": event.ID, "Attributes": attributes,
				},
				// Preserve legacy aliases for older Docker Engine API clients.
				"status": action, "id": event.ID,
				"scope": "local",
				"time":  event.Time.Unix(), "timeNano": event.Time.UnixNano(),
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

func matchesEventFilters(event state.Event, filters map[string][]string) bool {
	for key, values := range filters {
		matched := false
		for _, value := range values {
			switch key {
			case "type":
				matched = value == "container"
			case "event", "action":
				matched = value == dockerEventAction(event.Action) || value == event.Action
			case "container":
				matched = strings.HasPrefix(event.ID, value) || event.Name == value
			default:
				return false
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
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
	case "prune-completed":
		return "prune"
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

func requireMethodAllow(writer http.ResponseWriter, request *http.Request, method, allow string) bool {
	if request.Method == method {
		return true
	}
	writeMethodErrorFor(writer, allow)
	return false
}

func writeMethodErrorFor(writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	writeError(writer, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
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
