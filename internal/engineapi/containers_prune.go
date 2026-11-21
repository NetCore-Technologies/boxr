package engineapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/state"
)

type dockerContainerPruneResponse struct {
	ContainersDeleted []string `json:"ContainersDeleted"`
	SpaceReclaimed    int64    `json:"SpaceReclaimed"`
}

// handleContainerPrune implements Docker's POST /containers/prune endpoint.
// A container is re-read and removed through the state store so a concurrent
// start cannot turn a stale list result into deletion of a running process.
func (s *Server) handleContainerPrune(writer http.ResponseWriter, request *http.Request) {
	s.containerPruneMu.Lock()
	defer s.containerPruneMu.Unlock()

	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	now := time.Now()
	if err := validateDockerContainerPruneFilters(filters, now); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}

	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("list containers for prune: %w", err))
		return
	}

	result := dockerContainerPruneResponse{ContainersDeleted: make([]string, 0)}
	for _, record := range records {
		unlock := s.containerTransitions.lock(record.ID)
		current, getErr := state.Get(record.ID)
		if getErr != nil {
			unlock()
			continue
		}
		if current.Status == "running" || current.Status == "paused" || current.Status == "restarting" {
			unlock()
			continue
		}
		matched, err := matchesDockerContainerPruneFilters(current, filters, now)
		if err != nil {
			unlock()
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		if !matched {
			unlock()
			continue
		}
		record = current

		var removeErr error
		if record.Status == "created" {
			removeErr = state.RemoveCreated(record.ID)
		} else {
			removeErr = state.Remove(record.ID)
		}
		if removeErr != nil {
			// A concurrent explicit remove or start may have changed this
			// record since the initial snapshot. Re-check before deciding
			// whether the prune operation itself has failed.
			current, getErr := state.Get(record.ID)
			unlock()
			if getErr != nil {
				continue // another operation already removed the record
			}
			if current.Status == "running" || current.Status == "paused" || current.Status == "restarting" {
				continue
			}
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("remove container %s during prune: %w", record.ID, removeErr))
			return
		}
		unlock()
		result.ContainersDeleted = append(result.ContainersDeleted, record.ID)
	}

	// Boxr does not own a per-container writable-layer store yet, so it cannot
	// report Docker's filesystem-layer reclaim metric accurately. State records,
	// Engine API sidecars and log files are still removed by the state store.
	result.SpaceReclaimed = 0
	if err := state.RecordPruneEvent(result.SpaceReclaimed); err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("record container prune event: %w", err))
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func validateDockerContainerPruneFilters(filters map[string][]string, now time.Time) error {
	for key, values := range filters {
		switch key {
		case "until":
			if len(values) == 0 {
				return fmt.Errorf("container prune filter until must contain at least one timestamp")
			}
			if len(values) > 1 {
				return fmt.Errorf("container prune accepts only one until timestamp")
			}
			for _, value := range values {
				if strings.TrimSpace(value) == "" || len(value) > 256 {
					return fmt.Errorf("invalid container prune until filter %q", value)
				}
				if _, err := parseDockerNetworkUntil(value, now); err != nil {
					return fmt.Errorf("invalid container prune until filter %q: %w", value, err)
				}
			}
		case "label", "label!":
			if len(values) == 0 {
				return fmt.Errorf("container prune filter %s must contain at least one value", key)
			}
			for _, value := range values {
				labelKey, labelValue, _ := strings.Cut(value, "=")
				if strings.TrimSpace(labelKey) == "" || len(labelKey) > 128 || len(labelValue) > 4096 {
					return fmt.Errorf("invalid container label filter %q", value)
				}
				if strings.ContainsAny(labelKey+labelValue, "\x00\r\n") {
					return fmt.Errorf("invalid container label filter %q", value)
				}
			}
		default:
			return fmt.Errorf("unsupported container prune filter %q", key)
		}
	}
	return nil
}

func matchesDockerContainerPruneFilters(record state.Record, filters map[string][]string, now time.Time) (bool, error) {
	for key, values := range filters {
		matched := false
		switch key {
		case "until":
			for _, value := range values {
				cutoff, err := parseDockerNetworkUntil(value, now)
				if err != nil {
					return false, err
				}
				if record.CreatedAt.Before(cutoff) {
					matched = true
					break
				}
			}
		case "label":
			for _, value := range values {
				if dockerContainerHasLabel(record.Labels, value) {
					matched = true
					break
				}
			}
		case "label!":
			// Docker combines multiple negative label expressions into one
			// protected set. A container is excluded from prune only when
			// it satisfies every negative-label expression.
			allExcludedLabelsMatch := true
			for _, value := range values {
				if !dockerContainerHasLabel(record.Labels, value) {
					allExcludedLabelsMatch = false
					break
				}
			}
			matched = !allExcludedLabelsMatch
		default:
			return false, fmt.Errorf("unsupported container prune filter %q", key)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

func dockerContainerHasLabel(labels map[string]string, expression string) bool {
	key, expected, hasValue := strings.Cut(expression, "=")
	actual, exists := labels[key]
	return exists && (!hasValue || actual == expected)
}
