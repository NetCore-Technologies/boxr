package engineapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

const maxDockerContainerUpdateBodyBytes = 16 << 10

type dockerContainerUpdateRequest struct {
	Memory            *int64 `json:"Memory"`
	MemoryReservation *int64 `json:"MemoryReservation"`
	MemorySwap        *int64 `json:"MemorySwap"`
	NanoCPUs          *int64 `json:"NanoCpus"`
	PidsLimit         *int64 `json:"PidsLimit"`
}

func decodeDockerContainerUpdateRequest(request *http.Request) (dockerContainerUpdateRequest, error) {
	var input dockerContainerUpdateRequest
	data, err := io.ReadAll(io.LimitReader(request.Body, maxDockerContainerUpdateBodyBytes+1))
	if err != nil {
		return input, fmt.Errorf("read container update request: %w", err)
	}
	if len(data) == 0 || len(data) > maxDockerContainerUpdateBodyBytes {
		return input, fmt.Errorf("container update request must contain between 1 byte and %d bytes", maxDockerContainerUpdateBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("decode container update request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return input, fmt.Errorf("container update request must contain one JSON object")
	}
	return input, nil
}

func (s *Server) handleContainerUpdate(writer http.ResponseWriter, request *http.Request, rawID string) {
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("container update does not accept query parameters"))
		return
	}
	query, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}

	unlock := s.containerTransitions.lock(query)
	defer unlock()	input, err := decodeDockerContainerUpdateRequest(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}

	update := container.CgroupResourceUpdate{
		Memory:            input.Memory != nil,
		MemoryReservation: input.MemoryReservation != nil,
		MemorySwap:        input.MemorySwap != nil,
		NanoCPUs:          input.NanoCPUs != nil,
		PidsLimit:         input.PidsLimit != nil,
	}
	if !update.Memory && !update.MemoryReservation && !update.MemorySwap && !update.NanoCPUs && !update.PidsLimit {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("at least one supported resource field is required"))
		return
	}

	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status != "running" || record.PID <= 1 || record.ProcessStartTicks == 0 {
		writeError(writer, http.StatusConflict, fmt.Errorf("container must be running with a verifiable process identity before resources can be updated"))
		return
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("refusing resource update for an unverified process: %w", err))
		return
	}
	if s.verifyUpdatePID == nil {
		writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("container process verification is unavailable"))
		return
	}
	if err := s.verifyUpdatePID(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("refusing resource update for an unverified Boxr container: %w", err))
		return
	}
	if s.updateCgroupResources == nil {
		writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("live cgroup resource updates are unavailable"))
		return
	}

	options := record.Options
	if input.Memory != nil {
		if *input.Memory < 0 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("Memory cannot be negative"))
			return
		}
		options.MemoryLimitBytes = *input.Memory
	}
	if input.MemoryReservation != nil {
		if *input.MemoryReservation < 0 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("MemoryReservation cannot be negative"))
			return
		}
		options.MemoryLowBytes = *input.MemoryReservation
	}
	if input.MemorySwap != nil {
		switch {
		case *input.MemorySwap < -1:
			writeError(writer, http.StatusBadRequest, fmt.Errorf("MemorySwap must be -1 (unlimited), 0 (Docker default), or a positive total memory-plus-swap limit"))
			return
		case *input.MemorySwap == -1:
			options.MemorySwapMaxBytes = 0
			options.MemorySwapMaxSet = false
			options.MemorySwapDefaulted = false
			options.MemorySwapUnlimited = true
		case *input.MemorySwap == 0:
			options.MemorySwapMaxBytes = options.MemoryLimitBytes
			options.MemorySwapMaxSet = options.MemoryLimitBytes > 0
			options.MemorySwapDefaulted = true
			options.MemorySwapUnlimited = false
		case options.MemoryLimitBytes <= 0:
			writeError(writer, http.StatusBadRequest, fmt.Errorf("MemorySwap requires a positive Memory limit"))
			return
		case *input.MemorySwap < options.MemoryLimitBytes:
			writeError(writer, http.StatusBadRequest, fmt.Errorf("MemorySwap cannot be smaller than the effective Memory limit"))
			return
		default:
			options.MemorySwapMaxBytes = *input.MemorySwap - options.MemoryLimitBytes
			options.MemorySwapMaxSet = true
			options.MemorySwapDefaulted = false
			options.MemorySwapUnlimited = false
		}
	} else if input.Memory != nil {
		switch {
		case record.Options.MemorySwapDefaulted || (!record.Options.MemorySwapUnlimited && !record.Options.MemorySwapMaxSet && record.Options.MemorySwapMaxBytes == 0):
			options.MemorySwapMaxBytes = options.MemoryLimitBytes
			options.MemorySwapMaxSet = options.MemoryLimitBytes > 0
			options.MemorySwapDefaulted = true
		case record.Options.MemorySwapMaxSet || record.Options.MemorySwapMaxBytes > 0:
			previousTotal := record.Options.MemoryLimitBytes + record.Options.MemorySwapMaxBytes
			if options.MemoryLimitBytes > previousTotal {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("Memory cannot exceed the existing total MemorySwap limit; update MemorySwap in the same request"))
				return
			}
			options.MemorySwapMaxBytes = previousTotal - options.MemoryLimitBytes
		}
	}
	if options.MemoryLimitBytes > 0 && options.MemoryLowBytes >= options.MemoryLimitBytes && options.MemoryLowBytes > 0 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("MemoryReservation cannot exceed the effective Memory limit"))
		return
	}
	if input.NanoCPUs != nil {
		if *input.NanoCPUs < 0 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("NanoCpus cannot be negative"))
			return
		}
		options.CPUs = float64(*input.NanoCPUs) / 1e9
	}
	if input.PidsLimit != nil {
		if *input.PidsLimit < -1 {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("PidsLimit must be -1, 0, or a positive process limit"))
			return
		}
		options.PidsLimit = *input.PidsLimit
		if options.PidsLimit == -1 {
			options.PidsLimit = 0
		}
	}
	if input.Memory != nil && input.MemorySwap == nil && !record.Options.MemorySwapUnlimited {
		// A Docker memory change also changes the effective swap-only ceiling
		// whenever swap is defaulted or a total MemorySwap limit was configured.
		update.MemorySwap = true
	}
	if err := options.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid updated container resources: %w", err))
		return
	}

	if err := s.updateCgroupResources(record.PID, options, update); err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("apply live cgroup resource update: %w", err))
		return
	}
	if _, err := state.UpdateRunningOptions(record.ID, record.PID, record.ProcessStartTicks, options); err != nil {
		rollbackErr := s.updateCgroupResources(record.PID, record.Options, update)
		if rollbackErr != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("persist resource update: %v; restore prior cgroup limits: %w", err, rollbackErr))
			return
		}
		writeError(writer, http.StatusConflict, fmt.Errorf("container changed while applying resource update: %w", err))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"Warnings": []string{}})
}
