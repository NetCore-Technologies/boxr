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
	filtered, err := filterDockerVolumes(items, filters, records)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	result := make([]dockerVolumeResponse, 0, len(filtered))
	for _, item := range filtered {
		result = append(result, dockerVolume(item))
	}
	writeJSON(writer, http.StatusOK, dockerVolumeListResponse{
		Volumes: result, Warnings: []string{},
	})
}

func filterDockerVolumes(items []volume.Volume, filters map[string][]string, records []state.Record) ([]volume.Volume, error) {
	result := make([]volume.Volume, 0, len(items))
	for _, item := range items {
		include := true
		for key, values := range filters {
			if len(values) == 0 {
				return nil, fmt.Errorf("volume filter %q must contain at least one value", key)
			}
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
					if err != nil {
						return nil, err
					}
					matched = matched || wanted != dockerVolumeReferenced(item.Path, records)
				default:
					return nil, fmt.Errorf("unsupported volume filter %q", key)
				}
			}
			if !matched {
				include = false
				break
			}
		}
		if include {
			result = append(result, item)
		}
	}
	return result, nil
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
	var created volume.Volume
	if input.Name == "" {
		created, err = volume.CreateAnonymous(input.Labels)
	} else {
		created, err = volume.Create(input.Name, input.Labels)
	}
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


type dockerVolumePruneResponse struct {
	VolumesDeleted []string `json:"VolumesDeleted"`
	SpaceReclaimed int64    `json:"SpaceReclaimed"`
}

type measuredDockerVolume struct {
	item  volume.Volume
	bytes int64
}

func (s *Server) handleVolumePrune(writer http.ResponseWriter, request *http.Request) {
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()

	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	includeNamed, err := volumePruneIncludesNamed(filters)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	items, err := volume.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("list volumes for prune: %w", err))
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container volume references: %w", err))
		return
	}

	// Measure every candidate before deleting anything. A filesystem walk error
	// therefore cannot leave a half-pruned set just because the reclaim report
	// could not be calculated.
	candidates := make([]measuredDockerVolume, 0, len(items))
	for _, item := range items {
		if dockerVolumeReferenced(item.Path, records) {
			continue
		}
		if !includeNamed && !item.Anonymous {
			continue
		}
		matched, err := matchesDockerVolumePruneFilters(item, filters)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		if !matched {
			continue
		}
		usage, err := volume.Measure(item.Name)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("measure volume %q before prune: %w", item.Name, err))
			return
		}
		candidates = append(candidates, measuredDockerVolume{item: item, bytes: usage.TotalBytes})
	}

	result := dockerVolumePruneResponse{VolumesDeleted: make([]string, 0, len(candidates))}
	const maxInt64 = int64(^uint64(0) >> 1)
	for _, candidate := range candidates {
		if err := volume.Remove(candidate.item.Name); err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("remove volume %q during prune: %w", candidate.item.Name, err))
			return
		}
		result.VolumesDeleted = append(result.VolumesDeleted, candidate.item.Name)
		if candidate.bytes > 0 {
			if result.SpaceReclaimed > maxInt64-candidate.bytes {
				result.SpaceReclaimed = maxInt64
			} else {
				result.SpaceReclaimed += candidate.bytes
			}
		}
	}
	writeJSON(writer, http.StatusOK, result)
}

func volumePruneIncludesNamed(filters map[string][]string) (bool, error) {
	values, ok := filters["all"]
	if !ok {
		return false, nil
	}
	if len(values) == 0 {
		return false, fmt.Errorf("volume prune filter all must contain a boolean value")
	}
	includeNamed := false
	for _, value := range values {
		parsed, err := parseBooleanFilter(value)
		if err != nil {
			return false, fmt.Errorf("volume prune filter all: %w", err)
		}
		includeNamed = includeNamed || parsed
	}
	return includeNamed, nil
}

func matchesDockerVolumePruneFilters(item volume.Volume, filters map[string][]string) (bool, error) {
	for key, values := range filters {
		if key == "all" {
			continue
		}
		if key != "label" && key != "label!" {
			return false, fmt.Errorf("unsupported volume prune filter %q", key)
		}
		if len(values) == 0 {
			return false, fmt.Errorf("volume prune filter %q must contain at least one value", key)
		}
		negate := key == "label!"
		matched := false
		for _, value := range values {
			labelKey, expected, hasValue := strings.Cut(value, "=")
			if strings.TrimSpace(labelKey) == "" || len(labelKey) > 128 {
				return false, fmt.Errorf("invalid volume label filter %q", value)
			}
			actual, exists := item.Labels[labelKey]
			valueMatches := exists && (!hasValue || actual == expected)
			if negate {
				valueMatches = !valueMatches
			}
			matched = matched || valueMatches
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}
