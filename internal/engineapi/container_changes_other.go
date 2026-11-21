//go:build !linux

package engineapi

import (
	"fmt"
	"net/http"
)

func (s *Server) handleContainerChanges(writer http.ResponseWriter, request *http.Request, rawID string) {
	writeError(writer, http.StatusNotImplemented, fmt.Errorf("container filesystem changes require Linux OverlayFS"))
}
