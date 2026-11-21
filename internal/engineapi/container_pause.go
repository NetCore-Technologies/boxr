package engineapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/manit6752025/boxr/internal/state"
)

func (s *Server) handleContainerPause(writer http.ResponseWriter, request *http.Request, rawID string) {
	s.handleContainerPauseTransition(writer, request, rawID, true)
}

func (s *Server) handleContainerUnpause(writer http.ResponseWriter, request *http.Request, rawID string) {
	s.handleContainerPauseTransition(writer, request, rawID, false)
}

func (s *Server) handleContainerPauseTransition(writer http.ResponseWriter, request *http.Request, rawID string, pause bool) {
	action, expectedStatus, targetStatus := "pause", "running", "paused"
	if !pause {
		action, expectedStatus, targetStatus = "unpause", "paused", "running"
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("%s does not accept query parameters", action))
		return
	}
	id, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	record, err := state.Get(id)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status == targetStatus {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if record.Status != expectedStatus {
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s must be %s before %s (current state: %s)", record.ID, expectedStatus, action, record.Status))
		return
	}
	if record.PID <= 1 || record.ProcessStartTicks == 0 {
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s has no verifiable running process identity", record.ID))
		return
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("refusing to %s an unverified process: %w", action, err))
		return
	}
	if s.verifyUpdatePID == nil {
		writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("container process verification is unavailable"))
		return
	}
	if err := s.verifyUpdatePID(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("refusing to %s an unverified Boxr container: %w", action, err))
		return
	}
	if s.setCgroupFrozen == nil {
		writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("cgroup pause and resume are unavailable"))
		return
	}
	if err := s.setCgroupFrozen(record.PID, pause); err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("apply cgroup %s: %w", action, err))
		return
	}

	// Recheck identity after the kernel transition. Roll back if the workload
	// disappeared or its process identity changed while the cgroup was frozen.
	if err := state.VerifyProcessIdentity(record); err != nil {
		rollbackErr := s.setCgroupFrozen(record.PID, !pause)
		writeError(writer, http.StatusConflict, errors.Join(
			fmt.Errorf("container process changed during %s: %w", action, err), rollbackErr,
		))
		return
	}
	if err := s.verifyUpdatePID(record); err != nil {
		rollbackErr := s.setCgroupFrozen(record.PID, !pause)
		writeError(writer, http.StatusConflict, errors.Join(
			fmt.Errorf("container identity changed during %s: %w", action, err), rollbackErr,
		))
		return
	}
	if err := state.SetPaused(record.ID, record.PID, record.ProcessStartTicks, pause); err != nil {
		rollbackErr := s.setCgroupFrozen(record.PID, !pause)
		writeError(writer, http.StatusConflict, errors.Join(
			fmt.Errorf("persist container %s transition: %w", action, err), rollbackErr,
		))
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
