package engineapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/state"
)

// handleContainerAttach implements Docker's one-way container attach stream.
// Boxr currently persists stdout and stderr in a single combined log, so the
// stream is advertised as stdout; stdin/interactive container attachment is
// rejected instead of pretending that a detached workload has an input pipe.
func (s *Server) handleContainerAttach(writer http.ResponseWriter, request *http.Request, query string) {
	if request.Method != http.MethodPost {
		writeMethodErrorFor(writer, http.MethodPost)
		return
	}
	for key := range request.URL.Query() {
		switch key {
		case "logs", "stream", "stdin", "stdout", "stderr":
		default:
			writeError(writer, http.StatusBadRequest, fmt.Errorf("unsupported attach query parameter %q", key))
			return
		}
	}
	stdin, err := boolQuery(request, "stdin")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if stdin {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("container stdin attachment is unavailable; Boxr container processes do not currently expose a persistent stdin stream"))
		return
	}
	stdout, err := boolQuery(request, "stdout")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	stderr, err := boolQuery(request, "stderr")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	logs, err := boolQuery(request, "logs")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	outputStream := byte(1)
	if !stdout && stderr {
		// Combined output cannot be separated, but Docker clients requesting
		// stderr alone still need to receive the persisted combined stream.
		outputStream = 2
	}
	outputEnabled := stdout || stderr
	stream, err := boolQuery(request, "stream")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}

	var data []byte
	var offset int64
	if logs {
		data, offset, err = state.ReadLogsSnapshot(query, 0)
	} else {
		offset, err = state.LogOffset(query)
	}
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	var target io.Writer = writer
	var flusher http.Flusher
	var connection io.ReadWriteCloser
	if dockerExecUpgradeRequested(request) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("HTTP server does not support Docker attach stream hijacking"))
			return
		}
		conn, buffered, hijackErr := hijacker.Hijack()
		if hijackErr != nil {
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("hijack Docker attach stream: %w", hijackErr))
			return
		}
		if _, writeErr := buffered.WriteString("HTTP/1.1 101 UPGRADED\\r\\nContent-Type: application/vnd.docker.raw-stream\\r\\nConnection: Upgrade\\r\\nUpgrade: tcp\\r\\n\\r\\n"); writeErr != nil {
			_ = conn.Close()
			return
		}
		if flushErr := buffered.Flush(); flushErr != nil {
			_ = conn.Close()
			return
		}
		connection = conn
		target = conn
		defer connection.Close()
	} else {
		writer.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.WriteHeader(http.StatusOK)
		flusher, _ = writer.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if logs && outputEnabled && len(data) > 0 {
		if err := writeLogFrame(target, data, outputStream); err != nil {
			return
		}
	}
	if !stream {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			var buffer strings.Builder
			next, copyErr := state.CopyLogFrom(query, offset, &buffer)
			if copyErr != nil {
				return
			}
			if next < offset {
				// The bounded log may have been truncated/rotated while the
				// client was attached. CopyLogFrom resumes from byte zero.
				offset = next
			} else if next > offset {
				if outputEnabled && buffer.Len() > 0 {
					if err := writeLogFrame(target, []byte(buffer.String()), outputStream); err != nil {
						return
					}
				}
				offset = next
				if flusher != nil {
					flusher.Flush()
				}
			}
			record, getErr := state.Get(query)
			if getErr != nil || (record.Status != "running" && record.Status != "restarting") {
				return
			}
		}
	}
}
