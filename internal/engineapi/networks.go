package engineapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	boxrnetwork "github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/state"
)

type dockerNetworkIPAMConfig struct {
	Subnet     string            `json:"Subnet"`
	IPRange    string            `json:"IPRange"`
	Gateway    string            `json:"Gateway"`
	AuxAddress map[string]string `json:"AuxiliaryAddresses"`
}

type dockerNetworkIPAM struct {
	Driver  string                    `json:"Driver"`
	Config  []dockerNetworkIPAMConfig `json:"Config"`
	Options map[string]string         `json:"Options"`
}

type dockerNetworkCreateRequest struct {
	Name           string            `json:"Name"`
	CheckDuplicate bool              `json:"CheckDuplicate"`
	Driver         string            `json:"Driver"`
	Internal       bool              `json:"Internal"`
	Attachable     bool              `json:"Attachable"`
	Ingress        bool              `json:"Ingress"`
	EnableIPv6     bool              `json:"EnableIPv6"`
	IPAM           dockerNetworkIPAM `json:"IPAM"`
	Options        map[string]string `json:"Options"`
	Labels         map[string]string `json:"Labels"`
}

type dockerNetworkIPAMResponse struct {
	Driver  string                    `json:"Driver"`
	Config  []dockerNetworkIPAMConfig `json:"Config"`
	Options map[string]string         `json:"Options"`
}

type dockerNetworkResponse struct {
	Name       string                       `json:"Name"`
	ID         string                       `json:"Id"`
	Created    string                       `json:"Created"`
	Scope      string                       `json:"Scope"`
	Driver     string                       `json:"Driver"`
	EnableIPv6 bool                         `json:"EnableIPv6"`
	IPAM       dockerNetworkIPAMResponse    `json:"IPAM"`
	Internal   bool                         `json:"Internal"`
	Attachable bool                         `json:"Attachable"`
	Ingress    bool                         `json:"Ingress"`
	ConfigOnly bool                         `json:"ConfigOnly"`
	Options    map[string]string            `json:"Options"`
	Labels     map[string]string            `json:"Labels"`
	Containers map[string]map[string]string `json:"Containers"`
}

type dockerNetworkCreateResponse struct {
	ID      string `json:"Id"`
	Warning string `json:"Warning"`
}

func dockerNetwork(item boxrnetwork.Summary) dockerNetworkResponse {
	options := map[string]string{
		"com.docker.network.bridge.name":                 item.Bridge,
		"com.docker.network.bridge.enable_ip_masquerade": strconv.FormatBool(item.NAT),
	}
	return dockerNetworkResponse{
		Name: item.Name, ID: item.ID, Created: item.CreatedAt.UTC().Format(time.RFC3339Nano),
		Scope: "local", Driver: "bridge", EnableIPv6: false,
		IPAM: dockerNetworkIPAMResponse{
			Driver: "default",
			Config: []dockerNetworkIPAMConfig{{Subnet: item.Subnet, Gateway: item.Gateway}},
		},
		Internal: false, Attachable: true, Ingress: false, ConfigOnly: false,
		Options: options, Labels: item.Labels,
		Containers: make(map[string]map[string]string),
	}
}

func matchesDockerNetworkFilters(item boxrnetwork.Summary, filters map[string][]string) bool {
	for key, values := range filters {
		if len(values) == 0 {
			return false
		}
		matched := false
		for _, value := range values {
			switch key {
			case "id":
				matched = matched || strings.HasPrefix(item.ID, value)
			case "name":
				matched = matched || strings.Contains(item.Name, value)
			case "driver":
				matched = matched || value == item.Driver
			case "type":
				matched = matched || value == "custom"
			case "scope":
				matched = matched || value == "local"
			case "label":
				label, expected, hasValue := strings.Cut(value, "=")
				actual, exists := item.Labels[label]
				matched = matched || (exists && (!hasValue || actual == expected))
			case "dangling":
				if wanted, err := parseBooleanFilter(value); err == nil {
					matched = matched || wanted == (item.ContainerCount == 0)
				}
			default:
				return false
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (s *Server) handleNetworks(writer http.ResponseWriter, request *http.Request) {
	items, err := boxrnetwork.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("list networks: %w", err))
		return
	}
	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	result := make([]dockerNetworkResponse, 0, len(items))
	for _, item := range items {
		if matchesDockerNetworkFilters(item, filters) {
			result = append(result, dockerNetwork(item))
		}
	}
	writeJSON(writer, http.StatusOK, result)
}

func parseDockerNetworkCreate(data []byte) (string, boxrnetwork.CreateOptions, error) {
	var input dockerNetworkCreateRequest
	var options boxrnetwork.CreateOptions
	if len(data) == 0 || len(data) > maxCreateBodyBytes {
		return "", options, fmt.Errorf("network create request must be between 1 byte and %d bytes", maxCreateBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return "", options, fmt.Errorf("decode network create request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", options, fmt.Errorf("network create request must contain one JSON object")
	}
	if strings.TrimSpace(input.Name) == "" {
		return "", options, fmt.Errorf("network Name is required")
	}
	driver := input.Driver
	if driver == "" {
		driver = "bridge"
	}
	if driver != "bridge" {
		return "", options, fmt.Errorf("unsupported network driver %q; only bridge is supported", driver)
	}
	if input.Internal || input.Ingress || input.EnableIPv6 {
		return "", options, fmt.Errorf("Internal, Ingress and EnableIPv6 networks are not supported")
	}
	if input.IPAM.Driver != "" && input.IPAM.Driver != "default" {
		return "", options, fmt.Errorf("unsupported IPAM driver %q", input.IPAM.Driver)
	}
	if len(input.IPAM.Options) > 0 {
		return "", options, fmt.Errorf("custom IPAM options are not supported")
	}
	if len(input.IPAM.Config) != 1 {
		return "", options, fmt.Errorf("IPAM.Config must contain exactly one IPv4 subnet")
	}
	config := input.IPAM.Config[0]
	if config.Subnet == "" || config.IPRange != "" || len(config.AuxAddress) > 0 {
		return "", options, fmt.Errorf("IPAM requires Subnet; IPRange and AuxiliaryAddresses are not supported")
	}
	for key, value := range input.Options {
		switch key {
		case "com.docker.network.bridge.name":
			options.Bridge = value
		case "com.docker.network.bridge.enable_ip_masquerade":
			nat, err := parseBooleanFilter(value)
			if err != nil {
				return "", options, fmt.Errorf("network option %s: %w", key, err)
			}
			options.NAT = nat
		default:
			return "", options, fmt.Errorf("unsupported bridge network option %q", key)
		}
	}
	if _, ok := input.Options["com.docker.network.bridge.enable_ip_masquerade"]; !ok {
		options.NAT = true
	}
	options.Subnet = config.Subnet
	options.Gateway = config.Gateway
	return input.Name, options, nil
}

func (s *Server) handleNetworkCreate(writer http.ResponseWriter, request *http.Request) {
	data, err := io.ReadAll(io.LimitReader(request.Body, maxCreateBodyBytes+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("read network create request: %w", err))
		return
	}
	name, options, err := parseDockerNetworkCreate(data)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	// Labels are metadata on Boxr's managed bridge, and never influence
	// interface names, firewall rules or IPAM paths.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if value, ok := raw["Labels"]; ok {
			if err := json.Unmarshal(value, &options.Labels); err != nil {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("decode network labels: %w", err))
				return
			}
		}
	}
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	created, err := s.createNetwork(name, options)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "already in use") {
			writeError(writer, http.StatusConflict, err)
		} else {
			writeError(writer, http.StatusBadRequest, err)
		}
		return
	}
	writeJSON(writer, http.StatusCreated, dockerNetworkCreateResponse{ID: created.ID, Warning: ""})
}

func dockerNetworkWithContainers(item boxrnetwork.Network, records []state.Record) dockerNetworkResponse {
	summary := boxrnetwork.Summary{
		ID: item.ID, Name: item.Name, Driver: item.Driver, Bridge: item.Bridge,
		Subnet: item.Subnet, Gateway: item.Gateway, NAT: item.NAT,
		ContainerCount: len(item.Allocations), CreatedAt: item.CreatedAt,
		Labels: item.Labels,
	}
	result := dockerNetwork(summary)
	result.Containers = make(map[string]map[string]string, len(item.Allocations))
	names := make(map[string]string, len(records))
	for _, record := range records {
		name := record.Name
		if name == "" {
			name = record.ID
		}
		names[record.ID] = "/" + name
	}
	mask := ""
	if _, value, ok := strings.Cut(item.Subnet, "/"); ok {
		mask = "/" + value
	}
	for id, address := range item.Allocations {
		name := names[id]
		if name == "" {
			name = "/" + id
		}
		result.Containers[id] = map[string]string{
			"Name": name, "EndpointID": "", "MacAddress": "",
			"IPv4Address": address + mask, "IPv6Address": "",
		}
	}
	return result
}

func (s *Server) handleNetworkInspect(writer http.ResponseWriter, name string) {
	item, err := boxrnetwork.InspectDetails(name)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusInternalServerError, err)
		}
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("list containers for network inspection: %w", err))
		return
	}
	writeJSON(writer, http.StatusOK, dockerNetworkWithContainers(item, records))
}

func (s *Server) handleNetworkDelete(writer http.ResponseWriter, name string) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	if err := s.removeNetwork(name); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusConflict, err)
		}
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

type dockerNetworkConnectRequest struct {
	Container      string          `json:"Container"`
	EndpointConfig json.RawMessage `json:"EndpointConfig"`
}

type dockerNetworkDisconnectRequest struct {
	Container string `json:"Container"`
	Force     bool   `json:"Force"`
}

func decodeDockerNetworkBody(request *http.Request, target any) error {
	data, err := io.ReadAll(io.LimitReader(request.Body, maxCreateBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read network request: %w", err)
	}
	if len(data) == 0 || len(data) > maxCreateBodyBytes {
		return fmt.Errorf("network request must be between 1 byte and %d bytes", maxCreateBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode network request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("network request must contain one JSON object")
	}
	return nil
}

func validateDockerEndpointConfig(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("EndpointConfig must be an object")
	}
	if len(fields) != 0 {
		return fmt.Errorf("EndpointConfig aliases, static addresses, links, MAC addresses and driver options are not supported yet")
	}
	return nil
}

func (s *Server) handleNetworkConnect(writer http.ResponseWriter, request *http.Request, networkName string) {
	var input dockerNetworkConnectRequest
	if err := decodeDockerNetworkBody(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(input.Container) == "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("Container is required"))
		return
	}
	if err := validateDockerEndpointConfig(input.EndpointConfig); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	record, err := state.Get(input.Container)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status != "running" || record.PID <= 1 {
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s must be running to connect a network", record.ID))
		return
	}
	if record.Options.NetworkMode != "none" || record.Options.UserNamespace {
		writeError(writer, http.StatusConflict, fmt.Errorf("live network attachment requires a running container started with network mode none and without a user namespace"))
		return
	}
	if err := s.verifyNetworkPID(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("refuse to enter container PID %d: %w", record.PID, err))
		return
	}
	if _, err := s.joinNetwork(networkName, record.PID, record.ID); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusConflict, err)
		}
		return
	}
	writer.WriteHeader(http.StatusOK)
}

func (s *Server) handleNetworkDisconnect(writer http.ResponseWriter, request *http.Request, networkName string) {
	var input dockerNetworkDisconnectRequest
	if err := decodeDockerNetworkBody(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(input.Container) == "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("Container is required"))
		return
	}
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	record, err := state.Get(input.Container)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if err := s.leaveNetwork(networkName, record.ID); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusConflict, err)
		}
		return
	}
	writer.WriteHeader(http.StatusOK)
}

func verifyDockerContainerPID(record state.Record) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("live Docker network attachment requires Linux")
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		return err
	}
	if record.PID <= 1 {
		return fmt.Errorf("container PID must be greater than 1")
	}
	procDir := filepath.Join("/proc", strconv.Itoa(record.PID))
	currentExecutable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Boxr executable: %w", err)
	}
	currentInfo, err := os.Stat(currentExecutable)
	if err != nil {
		return fmt.Errorf("inspect Boxr executable: %w", err)
	}
	processInfo, err := os.Stat(filepath.Join(procDir, "exe"))
	if err != nil {
		return fmt.Errorf("inspect container process executable: %w", err)
	}
	if !os.SameFile(currentInfo, processInfo) {
		return fmt.Errorf("PID does not belong to this Boxr executable")
	}
	commandLine, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return fmt.Errorf("read container process command line: %w", err)
	}
	arguments := strings.Split(strings.TrimSuffix(string(commandLine), "\x00"), "\x00")
	if len(arguments) < 3 || arguments[1] != "__boxr-init" {
		return fmt.Errorf("PID is not a Boxr container namespace helper")
	}
	if _, err := os.Stat(filepath.Join(procDir, "ns", "net")); err != nil {
		return fmt.Errorf("inspect container network namespace: %w", err)
	}
	return nil
}

type dockerNetworkPruneResponse struct {
	NetworksDeleted []string `json:"NetworksDeleted"`
}

func (s *Server) handleNetworkPrune(writer http.ResponseWriter, request *http.Request) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()

	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	now := time.Now()
	if err := validateDockerNetworkPruneFilters(filters, now); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	items, err := boxrnetwork.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("list networks for prune: %w", err))
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container network references: %w", err))
		return
	}
	result := dockerNetworkPruneResponse{NetworksDeleted: make([]string, 0)}
	for _, item := range items {
		if item.ContainerCount > 0 || dockerNetworkReferencedByRecords(item, records) {
			continue
		}
		matched, err := matchesDockerNetworkPruneFilters(item, filters, now)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		if !matched {
			continue
		}
		if err := s.removeNetwork(item.Name); err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("remove unused network %q: %w", item.Name, err))
			return
		}
		result.NetworksDeleted = append(result.NetworksDeleted, item.Name)
	}
	writeJSON(writer, http.StatusOK, result)
}

func dockerNetworkReferencedByRecords(item boxrnetwork.Summary, records []state.Record) bool {
	for _, record := range records {
		if record.Options.NetworkName == item.Name || record.Options.NetworkName == item.ID {
			return true
		}
		// The implicit Docker bridge is a live reference even when the caller
		// did not explicitly name it in HostConfig.NetworkMode.
		if item.Name == "bridge" && record.Options.NetworkMode == "bridge" && record.Options.NetworkName == "" {
			return true
		}
	}
	return false
}

func validateDockerNetworkPruneFilters(filters map[string][]string, now time.Time) error {
	for key, values := range filters {
		switch key {
		case "until":
			if len(values) == 0 {
				return fmt.Errorf("network prune filter until must contain at least one timestamp")
			}
			for _, value := range values {
				if _, err := parseDockerNetworkUntil(value, now); err != nil {
					return fmt.Errorf("invalid network prune until filter %q: %w", value, err)
				}
			}
		case "label", "label!":
			if len(values) == 0 {
				return fmt.Errorf("network prune filter %s must contain at least one value", key)
			}
			for _, value := range values {
				labelKey, _, _ := strings.Cut(value, "=")
				if strings.TrimSpace(labelKey) == "" || len(labelKey) > 128 {
					return fmt.Errorf("invalid network label filter %q", value)
				}
			}
		default:
			return fmt.Errorf("unsupported network prune filter %q", key)
		}
	}
	return nil
}

func matchesDockerNetworkPruneFilters(item boxrnetwork.Summary, filters map[string][]string, now time.Time) (bool, error) {
	for key, values := range filters {
		matched := false
		switch key {
		case "until":
			for _, value := range values {
				cutoff, err := parseDockerNetworkUntil(value, now)
				if err != nil {
					return false, err
				}
				if item.CreatedAt.Before(cutoff) {
					matched = true
					break
				}
			}
		case "label", "label!":
			negate := key == "label!"
			for _, value := range values {
				labelKey, expected, hasValue := strings.Cut(value, "=")
				actual, exists := item.Labels[labelKey]
				valueMatches := exists && (!hasValue || actual == expected)
				if negate {
					valueMatches = !valueMatches
				}
				if valueMatches {
					matched = true
					break
				}
			}
		default:
			return false, fmt.Errorf("unsupported network prune filter %q", key)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

func parseDockerNetworkUntil(value string, now time.Time) (time.Time, error) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(seconds, 0), nil
	}
	if secondsText, fraction, ok := strings.Cut(value, "."); ok && fraction != "" && len(fraction) <= 9 {
		seconds, secErr := strconv.ParseInt(secondsText, 10, 64)
		nanoseconds, nanoErr := strconv.ParseUint(fraction, 10, 32)
		if secErr == nil && nanoErr == nil {
			for digits := len(fraction); digits < 9; digits++ {
				nanoseconds *= 10
			}
			if seconds < 0 && nanoseconds > 0 {
				return time.Unix(seconds-1, int64(1_000_000_000-nanoseconds)), nil
			}
			return time.Unix(seconds, int64(nanoseconds)), nil
		}
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return now.Add(-duration), nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
	} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("expected Unix timestamp, RFC3339 timestamp, local date/time, or Go duration")
}
