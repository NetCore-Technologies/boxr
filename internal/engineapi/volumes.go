package engineapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

type dockerVolumeCreateRequest struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	DriverOpts map[string]string `json:"DriverOpts"`
	Labels     map[string]string `json:"Labels"`
}

type dockerVolumeResponse struct {
	CreatedAt  string            `json:"CreatedAt"`
	Driver     string            `json:"Driver"`
	Labels     map[string]string `json:"Labels"`
	Mountpoint string            `json:"Mountpoint"`
	Name       string            `json:"Name"`
	Options    map[string]string `json:"Options"`
	Scope      string            `json:"Scope"`
	UsageData  any               `json:"UsageData"`
}

type dockerVolumeListResponse struct {
	Volumes  []dockerVolumeResponse `json:"Volumes"`
	Warnings []string               `json:"Warnings"`
}

func dockerVolume(item volume.Volume) dockerVolumeResponse {
	return dockerVolumeResponse{
		CreatedAt:  item.CreatedAt.UTC().Format(time.RFC3339Nano),
		Driver:     "local",
		Labels:     item.Labels,
		Mountpoint: item.Path,
		Name:       item.Name,
		Options:    nil,
		Scope:      "local",
		UsageData:  nil,
	}
}

func (s *Server) handleVolumes(writer http.ResponseWriter, request *http.Request) {
	items, err := volume.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("list volumes: %w", err))
		return
	}
	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect volume users: %w", err))
		return
	}
	result := make([]dockerVolumeResponse, 0, len(items))
	for _, item := range items {
		if !matchesDockerVolumeFilters(item, filters, records) {
			continue
		}
		result = append(result, dockerVolume(item))
	}
	writeJSON(writer, http.StatusOK, dockerVolumeListResponse{
		Volumes: result, Warnings: []string{},
	})
}

func matchesDockerVolumeFilters(item volume.Volume, filters map[string][]string, records []state.Record) bool {
	for key, values := range filters {
		matched := false
		for _, value := range values {
			switch key {
			case "name":
				matched = matched || strings.Contains(item.Name, value)
			case "label":
				label, expected, hasValue := strings.Cut(value, "=")
				actual, exists := item.Labels[label]
				matched = matched || (exists && (!hasValue || actual == expected))
			case "driver":
				matched = matched || value == "local"
			case "scope":
				matched = matched || value == "local"
			case "dangling":
				wanted, err := parseBooleanFilter(value)
				if err == nil {
					matched = matched || wanted != dockerVolumeReferenced(item.Path, records)
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

func parseBooleanFilter(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, fmt.Errorf("boolean filter must be true or false")
	}
}

func dockerVolumeReferenced(path string, records []state.Record) bool {
	for _, record := range records {
		for _, mount := range record.Options.BindMounts {
			if mount.Source == path {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleVolumeCreate(writer http.ResponseWriter, request *http.Request) {
	data, err := io.ReadAll(io.LimitReader(request.Body, maxCreateBodyBytes+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("read volume create request: %w", err))
		return
	}
	if len(data) == 0 || len(data) > maxCreateBodyBytes {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("volume create request must be between 1 byte and %d bytes", maxCreateBodyBytes))
		return
	}
	var input dockerVolumeCreateRequest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("decode volume create request: %w", err))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("volume create request must contain one JSON object"))
		return
	}
	if input.Name == "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("volume Name is required"))
		return
	}
	if input.Driver == "" {
		input.Driver = "local"
	}
	if input.Driver != "local" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("unsupported volume driver %q; only the local driver is available", input.Driver))
		return
	}
	if len(input.DriverOpts) > 0 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("local volume DriverOpts are not supported"))
		return
	}
	created, err := volume.Create(input.Name, input.Labels)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(writer, http.StatusConflict, err)
		} else {
			writeError(writer, http.StatusBadRequest, err)
		}
		return
	}
	writeJSON(writer, http.StatusCreated, dockerVolume(created))
}

func (s *Server) handleVolumeInspect(writer http.ResponseWriter, name string) {
	item, err := volume.Get(name)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			writeError(writer, http.StatusNotFound, err)
		} else {
			writeError(writer, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(writer, http.StatusOK, dockerVolume(item))
}

func (s *Server) handleVolumeDelete(writer http.ResponseWriter, name string) {
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()
	item, err := volume.Get(name)
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
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect volume users: %w", err))
		return
	}
	for _, record := range records {
		for _, mount := range record.Options.BindMounts {
			if mount.Source == item.Path {
				writeError(writer, http.StatusConflict, fmt.Errorf("volume %q is referenced by container %s", name, record.ID))
				return
			}
		}
	}
	if err := volume.Remove(name); err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
