package engineapi

import (
	"fmt"
	"net/http"
	"strings"

	imageStore "github.com/manit6752025/boxr/internal/image"
)

func (s *Server) handleImageHistory(writer http.ResponseWriter, rawReference string) {
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
	if image.Layer.Digest == "" || image.Layer.Size < 0 {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("image %q has incomplete rootfs layer metadata", reference))
		return
	}
	// Boxr stores a single flattened rootfs snapshot per image. Return exactly
	// that layer; never fabricate the Dockerfile instruction history that was
	// not retained by the OCI image config.
	writeJSON(writer, http.StatusOK, []map[string]any{{
		"Id":        image.Layer.Digest,
		"Created":   image.Created.Unix(),
		"CreatedBy": "boxr rootfs snapshot",
		"Tags":      []string{image.Reference},
		"Size":      image.Layer.Size,
		"Comment":   "Boxr stores a flattened rootfs snapshot; original Docker layer history is not preserved.",
	}})
}
