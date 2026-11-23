package engineapi

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/state"
)

// handleContainerAttach implements Docker's container attach stream. Boxr
// persists stdout and stderr together; non-TTY streams use Docker multiplex
// framing while TTY streams remain raw, as they do in the Engine API.
func (s *Server) handleContainerAttach(writer http.ResponseWriter, request *http.Request, query string) {
	if request.Method != http.MethodPost {
		writeMethodErrorFor(writer, http.MethodPost)
		return
	}
	record, err := state.Get(query)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
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
	var stdinConnection io.WriteCloser
	if stdin {
		if !stream || !dockerExecUpgradeRequested(request) {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("stdin attachment requires stream=1 and a Docker TCP upgrade"))
			return
		}
		if !record.Options.OpenStdin {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("container was not created with OpenStdin"))
			return
		}
		if record.Status != "running" {
			writeError(writer, http.StatusConflict, fmt.Errorf("container stdin is only available while the container is running"))
			return
		}
		stdinConnection, err = openDockerContainerStdin(record.ID)
		if err != nil {
			writeError(writer, http.StatusConflict, err)
			return
		}
		defer stdinConnection.Close()
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
	var inputDone <-chan struct{}
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
		if _, writeErr := buffered.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); writeErr != nil {
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
		if stdinConnection != nil {
			done := make(chan struct{})
			inputDone = done
			go func() {
				defer close(done)
				_, _ = io.Copy(stdinConnection, conn)
				_ = stdinConnection.Close()
			}()
		}
		if logs && outputEnabled && len(data) > 0 {
			if err := writeContainerLogStream(target, data, outputStream, record.Options.TTY); err != nil {
				return
			}
		}
	} else {
		writer.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.WriteHeader(http.StatusOK)
		flusher, _ = writer.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if !dockerExecUpgradeRequested(request) && logs && outputEnabled && len(data) > 0 {
		if err := writeContainerLogStream(target, data, outputStream, record.Options.TTY); err != nil {
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
		case <-inputDone:
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
					if err := writeContainerLogStream(target, []byte(buffer.String()), outputStream, record.Options.TTY); err != nil {
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

func writeContainerLogStream(writer io.Writer, data []byte, stream byte, tty bool) error {
	if tty {
		return writeDockerExecBytes(writer, data)
	}
	return writeLogFrame(writer, data, stream)
}
