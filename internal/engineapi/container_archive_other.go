//go:build !linux

package engineapi

import (
	"fmt"
	"net/http"
)

func (s *Server) handleContainerArchive(writer http.ResponseWriter, request *http.Request, rawID string) {
	writeError(writer, http.StatusNotImplemented, fmt.Errorf("container archive copy requires Linux"))
}
