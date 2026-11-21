package engineapi

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

type dockerImagePruneDeleted struct {
	Untagged string `json:"Untagged,omitempty"`
	Deleted  string `json:"Deleted,omitempty"`
}

type dockerImagePruneResponse struct {
	ImagesDeleted []dockerImagePruneDeleted `json:"ImagesDeleted"`
	SpaceReclaimed int64                    `json:"SpaceReclaimed"`
}

func (s *Server) handleImagePrune(writer http.ResponseWriter, request *http.Request) {
	filters, err := parseFilters(request.URL.Query().Get("filters"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	now := time.Now()
	if err := validateDockerImagePruneFilters(filters, now); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}

	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	// Verify every current image and blob before changing tags. This prevents a
	// prune request from deleting some tags before discovering corrupt store data.
	if _, err := imageStore.Usage(); err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("verify image store before prune: %w", err))
		return
	}
	images, err := imageStore.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("list images for prune: %w", err))
		return
	}
	records, err := state.List(true)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("inspect container image references: %w", err))
		return
	}

	byReference := make(map[string]imageStore.Image, len(images))
	for _, item := range images {
		byReference[item.Reference] = item
	}
	protectedDigests := make(map[string]struct{})
	for _, record := range records {
		reference := record.Labels["io.boxr.image"]
		if image, ok := byReference[reference]; ok {
			protectedDigests[image.Digest] = struct{}{}
		}
	}

	removeTagged, err := dockerImagePruneIncludesTagged(filters)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	removeReferences := make([]string, 0, len(images))
	for _, item := range images {
		if !removeTagged {
			// Boxr's OCI index contains named references only; it does not keep
			// untagged dangling image records. The default dangling=true mode
			// therefore keeps every tag and only reclaims orphaned blobs below.
			continue
		}
		if _, protected := protectedDigests[item.Digest]; protected {
			continue
		}
		matched, err := matchesDockerImagePruneFilters(item, filters, now)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		if matched {
			removeReferences = append(removeReferences, item.Reference)
		}
	}

	result := dockerImagePruneResponse{
		ImagesDeleted: make([]dockerImagePruneDeleted, 0, len(removeReferences)),
	}
	if len(removeReferences) > 0 {
		if err := imageStore.RemoveMany(removeReferences); err != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("remove unused image tags: %w", err))
			return
		}
		for _, reference := range removeReferences {
			result.ImagesDeleted = append(result.ImagesDeleted, dockerImagePruneDeleted{Untagged: reference})
		}
	}

	blobs, err := imageStore.PruneDetailed(false)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("reclaim unreferenced image blobs: %w", err))
		return
	}
	for _, blob := range blobs {
		result.ImagesDeleted = append(result.ImagesDeleted, dockerImagePruneDeleted{Deleted: blob.Digest})
		if blob.SizeBytes > 0 {
			if result.SpaceReclaimed > math.MaxInt64-blob.SizeBytes {
				result.SpaceReclaimed = math.MaxInt64
			} else {
				result.SpaceReclaimed += blob.SizeBytes
			}
		}
	}
	writeJSON(writer, http.StatusOK, result)
}

func validateDockerImagePruneFilters(filters map[string][]string, now time.Time) error {
	for key, values := range filters {
		switch key {
		case "dangling":
			if len(values) == 0 {
				return fmt.Errorf("image prune filter dangling must contain a boolean value")
			}
			for _, value := range values {
				if _, err := parseBooleanFilter(value); err != nil {
					return fmt.Errorf("invalid image prune dangling filter %q: %w", value, err)
				}
			}
		case "until":
			if len(values) == 0 {
				return fmt.Errorf("image prune filter until must contain at least one timestamp")
			}
			for _, value := range values {
				if _, err := parseDockerNetworkUntil(value, now); err != nil {
					return fmt.Errorf("invalid image prune until filter %q: %w", value, err)
				}
			}
		case "label", "label!":
			if len(values) == 0 {
				return fmt.Errorf("image prune filter %s must contain at least one value", key)
			}
			for _, value := range values {
				labelKey, _, _ := strings.Cut(value, "=")
				if strings.TrimSpace(labelKey) == "" || len(labelKey) > 128 {
					return fmt.Errorf("invalid image label filter %q", value)
				}
			}
		default:
			return fmt.Errorf("unsupported image prune filter %q", key)
		}
	}
	return nil
}

func dockerImagePruneIncludesTagged(filters map[string][]string) (bool, error) {
	values, ok := filters["dangling"]
	if !ok {
		return false, nil
	}
	for _, value := range values {
		dangling, err := parseBooleanFilter(value)
		if err != nil {
			return false, err
		}
		if !dangling {
			// A tag is not dangling, so it matches dangling=false.
			return true, nil
		}
	}
	return false, nil
}

func matchesDockerImagePruneFilters(item imageStore.Image, filters map[string][]string, now time.Time) (bool, error) {
	for key, values := range filters {
		matched := false
		switch key {
		case "dangling":
			for _, value := range values {
				dangling, err := parseBooleanFilter(value)
				if err != nil {
					return false, err
				}
				if !dangling {
					matched = true
					break
				}
			}
		case "until":
			for _, value := range values {
				cutoff, err := parseDockerNetworkUntil(value, now)
				if err != nil {
					return false, err
				}
				if item.Created.Before(cutoff) {
					matched = true
					break
				}
			}
		case "label", "label!":
			negate := key == "label!"
			for _, value := range values {
				labelKey, expected, hasValue := strings.Cut(value, "=")
				actual, exists := item.RuntimeConfig.Labels[labelKey]
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
			return false, fmt.Errorf("unsupported image prune filter %q", key)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}
