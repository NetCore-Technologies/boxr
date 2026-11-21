//go:build !linux

package engineapi

import (
	"fmt"
	"net/http"
)

func (s *Server) handleContainerExport(writer http.ResponseWriter, _ *http.Request, _ string) {
	writeError(writer, http.StatusNotImplemented, fmt.Errorf("container filesystem export currently requires Linux"))
}
