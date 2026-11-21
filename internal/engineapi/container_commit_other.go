//go:build !linux

package engineapi

import (
	"fmt"
	"net/http"
)

func (s *Server) handleContainerCommit(writer http.ResponseWriter, _ *http.Request) {
	writeError(writer, http.StatusNotImplemented, fmt.Errorf("container image commit currently requires Linux"))
}
