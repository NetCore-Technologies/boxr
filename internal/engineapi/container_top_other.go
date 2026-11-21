//go:build !linux

package engineapi

import (
	"fmt"
	"net/http"
)

func (s *Server) handleContainerTop(writer http.ResponseWriter, _ *http.Request, _ string) {
	writeError(writer, http.StatusNotImplemented, fmt.Errorf("container top is supported only on Linux"))
}
