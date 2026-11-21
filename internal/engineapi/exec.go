package engineapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

const (
	maxDockerExecBodyBytes    = 64 << 10
	maxDockerExecSessions     = 1024
	dockerExecSessionLifetime = time.Hour
)

type dockerExecCreateConfig struct {
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	Tty          bool
	Env          []string
	Cmd          []string
	Privileged   bool
	User         string
	WorkingDir   string
	ConsoleSize  []uint
	DetachKeys   string
}

type dockerExecStartConfig struct {
	Detach      bool
	Tty         bool
	ConsoleSize []uint
}

type dockerExecSession struct {
	ID          string
	ContainerID string
	Config      dockerExecCreateConfig
	CreatedAt   time.Time

	mu         sync.Mutex
	terminal   *os.File
	Started    bool
	Running    bool
	StartedAt  *time.Time
	FinishedAt *time.Time
	ExitCode   *int
	PID        int
	Removed    bool
}

type dockerExecRunnerFunc func(context.Context, string, dockerExecCreateConfig, io.Reader, io.Writer, io.Writer, func(int), func(*os.File)) (int, error)

// dockerExecRunner uses Boxr's native exec command so PID-identity checks,
// namespace entry and cgroup attachment remain part of the execution path.
var dockerExecRunner dockerExecRunnerFunc = executeDockerExec

func executeDockerExec(ctx context.Context, containerID string, config dockerExecCreateConfig, stdin io.Reader, stdout, stderr io.Writer, onStart func(int), onTTY func(*os.File)) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 125, fmt.Errorf("resolve Boxr executable: %w", err)
	}
	arguments := []string{"exec"}
	if config.User != "" {
		arguments = append(arguments, "--user", config.User)
	}
	if config.WorkingDir != "" {
		arguments = append(arguments, "--workdir", config.WorkingDir)
	}
	for _, value := range config.Env {
		arguments = append(arguments, "--env", value)
	}
	arguments = append(arguments, containerID)
	arguments = append(arguments, config.Cmd...)

	command := exec.CommandContext(ctx, executable, arguments...)
	if config.Tty {
		return runDockerExecTTY(command, config, stdin, stdout, onStart, onTTY)
	}
	command.Stdin = stdin
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		return 125, fmt.Errorf("start Boxr namespace-aware exec: %w", err)
	}
	if onStart != nil {
		onStart(command.Process.Pid)
	}
	if err := command.Wait(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode(), nil
		}
		return 125, fmt.Errorf("wait for Boxr exec: %w", err)
	}
	return 0, nil
}

func (s *Server) handleContainerExecCreate(writer http.ResponseWriter, request *http.Request, rawContainerID string) {
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("container exec create does not accept query parameters"))
		return
	}
	containerID, err := url.PathUnescape(rawContainerID)
	if err != nil || containerID == "" || strings.ContainsAny(containerID, "/\\\x00\r\n ") {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid container identifier"))
		return
	}
	config, err := decodeDockerExecCreateConfig(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if err := validateDockerExecCreateConfig(config); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	record, err := state.Get(containerID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if err := container.ValidateUserSpecForNamespace(config.User, record.Options.UserNamespace); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("invalid exec User: %w", err))
		return
	}
	if config.User == "" {
		config.User = record.Options.User
	}
	if record.Status != "running" || record.PID <= 1 || !state.ProcessAlive(record.PID) {
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s must be running to create an exec session", record.ID))
		return
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("refusing exec for stale container process: %w", err))
		return
	}
	if s.verifyExecPID == nil {
		writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("container process verification is unavailable"))
		return
	}
	if err := s.verifyExecPID(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("refusing exec for an unverified Boxr container process: %w", err))
		return
	}
	id, err := newDockerExecID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("create exec identifier: %w", err))
		return
	}
	session := &dockerExecSession{ID: id, ContainerID: record.ID, Config: config, CreatedAt: time.Now().UTC()}
	if err := s.storeDockerExecSession(session); err != nil {
		writeError(writer, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]string{"Id": id})
}

func decodeDockerExecCreateConfig(request *http.Request) (dockerExecCreateConfig, error) {
	var config dockerExecCreateConfig
	data, err := io.ReadAll(io.LimitReader(request.Body, maxDockerExecBodyBytes+1))
	if err != nil {
		return config, fmt.Errorf("read exec create request: %w", err)
	}
	if len(data) == 0 || len(data) > maxDockerExecBodyBytes {
		return config, fmt.Errorf("exec create request must contain between 1 byte and %d bytes", maxDockerExecBodyBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("decode exec create request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return config, fmt.Errorf("exec create request must contain one JSON object")
	}
	return config, nil
}

func validateDockerExecCreateConfig(config dockerExecCreateConfig) error {
	if len(config.Cmd) == 0 || len(config.Cmd) > 256 {
		return fmt.Errorf("exec Cmd must contain between 1 and 256 arguments")
	}
	var commandBytes int
	for index, argument := range config.Cmd {
		if argument == "" || strings.ContainsAny(argument, "\x00\r\n") {
			return fmt.Errorf("exec Cmd[%d] must be non-empty and contain no NUL or newline", index)
		}
		commandBytes += len(argument)
	}
	if commandBytes > 1<<20 {
		return fmt.Errorf("exec command exceeds 1 MiB")
	}

	if config.Privileged {
		return fmt.Errorf("Privileged exec is not supported")
	}
	if err := container.ValidateUserSpec(config.User); err != nil {
		return fmt.Errorf("User: %w", err)
	}
	if config.WorkingDir != "" {
		if !path.IsAbs(config.WorkingDir) || strings.ContainsAny(config.WorkingDir, "\x00\r\n") {
			return fmt.Errorf("WorkingDir must be an absolute container path without NUL or newlines")
		}
		for _, component := range strings.Split(config.WorkingDir, "/") {
			if component == ".." {
				return fmt.Errorf("WorkingDir cannot contain '..'")
			}
		}
	}
	if len(config.Env) > 128 {
		return fmt.Errorf("exec Env supports at most 128 entries")
	}
	totalEnvironmentBytes := 0
	for index, entry := range config.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !validDockerExecEnvironmentKey(key) || len(entry) > 8192 || strings.ContainsAny(entry, "\x00\r\n") {
			return fmt.Errorf("exec Env[%d] must be a bounded KEY=VALUE entry with a valid key", index)
		}
		totalEnvironmentBytes += len(entry)
	}
	if totalEnvironmentBytes > 1<<20 {
		return fmt.Errorf("exec environment exceeds 1 MiB")
	}
	if len(config.ConsoleSize) != 0 {
		if !config.Tty || len(config.ConsoleSize) != 2 {
			return fmt.Errorf("ConsoleSize requires a TTY and exactly [height,width]")
		}
		if config.ConsoleSize[0] == 0 || config.ConsoleSize[1] == 0 || config.ConsoleSize[0] > 65535 || config.ConsoleSize[1] > 65535 {
			return fmt.Errorf("ConsoleSize dimensions must be between 1 and 65535")
		}
	}
	if config.DetachKeys != "" {
		return fmt.Errorf("DetachKeys requires an attached stdin or TTY stream, which is not supported yet")
	}
	return nil
}

func validDockerExecEnvironmentKey(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character == '_' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) storeDockerExecSession(session *dockerExecSession) error {
	now := time.Now()
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.execSessions == nil {
		s.execSessions = make(map[string]*dockerExecSession)
	}
	for id, existing := range s.execSessions {
		existing.mu.Lock()
		expired := !existing.Running && now.Sub(existing.CreatedAt) > dockerExecSessionLifetime
		existing.mu.Unlock()
		if expired {
			delete(s.execSessions, id)
		}
	}
	if len(s.execSessions) >= maxDockerExecSessions {
		return fmt.Errorf("exec session limit (%d) reached; wait for old sessions to expire", maxDockerExecSessions)
	}
	s.execSessions[session.ID] = session
	return nil
}

func newDockerExecID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Server) dockerExecSession(id string) (*dockerExecSession, bool) {
	if len(id) != 64 {
		return nil, false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return nil, false
	}
	s.execMu.Lock()
	session, ok := s.execSessions[id]
	s.execMu.Unlock()
	return session, ok
}

func (s *Server) removeDockerExecSession(id string) error {
	if len(id) != 64 {
		return fmt.Errorf("invalid exec identifier")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return fmt.Errorf("invalid exec identifier")
	}
	s.execMu.Lock()
	defer s.execMu.Unlock()
	session, ok := s.execSessions[id]
	if !ok {
		return fmt.Errorf("exec session %q not found", id)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.Running {
		return fmt.Errorf("exec session %q is running", id)
	}
	session.Removed = true
	delete(s.execSessions, id)
	return nil
}

func (s *Server) handleExecDelete(writer http.ResponseWriter, id string) {
	if err := s.removeDockerExecSession(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(writer, http.StatusNotFound, err)
		} else if strings.Contains(err.Error(), "running") {
			writeError(writer, http.StatusConflict, err)
		} else {
			writeError(writer, http.StatusBadRequest, err)
		}
		return
	}
	writer.WriteHeader(http.StatusOK)
}

func decodeDockerExecStartConfig(request *http.Request) (dockerExecStartConfig, error) {
	var config dockerExecStartConfig
	data, err := io.ReadAll(io.LimitReader(request.Body, maxDockerExecBodyBytes+1))
	if err != nil {
		return config, fmt.Errorf("read exec start request: %w", err)
	}
	if len(data) == 0 {
		return config, nil
	}
	if len(data) > maxDockerExecBodyBytes {
		return config, fmt.Errorf("exec start request exceeds %d bytes", maxDockerExecBodyBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("decode exec start request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return config, fmt.Errorf("exec start request must contain one JSON object")
	}
	if len(config.ConsoleSize) != 0 {
		if !config.Tty || len(config.ConsoleSize) != 2 || config.ConsoleSize[0] == 0 || config.ConsoleSize[1] == 0 ||
			config.ConsoleSize[0] > 65535 || config.ConsoleSize[1] > 65535 {
			return config, fmt.Errorf("ConsoleSize requires a TTY and dimensions between 1 and 65535")
		}
	}
	return config, nil
}

func dockerExecUpgradeRequested(request *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "tcp") {
		return false
	}
	for _, token := range strings.Split(request.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

func (s *Server) handleExecStart(writer http.ResponseWriter, request *http.Request, id string) {
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("exec start does not accept query parameters"))
		return
	}
	session, ok := s.dockerExecSession(id)
	if !ok {
		writeError(writer, http.StatusNotFound, fmt.Errorf("exec session %q not found", id))
		return
	}
	startConfig, err := decodeDockerExecStartConfig(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	upgrade := dockerExecUpgradeRequested(request) && !startConfig.Detach

	session.mu.Lock()
	if session.Removed {
		session.mu.Unlock()
		writeError(writer, http.StatusNotFound, fmt.Errorf("exec session %q not found", id))
		return
	}
	if session.Started {
		session.mu.Unlock()
		writeError(writer, http.StatusConflict, fmt.Errorf("exec session %q has already been started", id))
		return
	}
	if startConfig.Tty != session.Config.Tty {
		session.mu.Unlock()
		writeError(writer, http.StatusBadRequest, fmt.Errorf("exec start Tty must match the Tty setting supplied at exec create"))
		return
	}
	if len(startConfig.ConsoleSize) != 0 {
		if !session.Config.Tty || len(startConfig.ConsoleSize) != 2 ||
			startConfig.ConsoleSize[0] == 0 || startConfig.ConsoleSize[1] == 0 ||
			startConfig.ConsoleSize[0] > 65535 || startConfig.ConsoleSize[1] > 65535 {
			session.mu.Unlock()
			writeError(writer, http.StatusBadRequest, fmt.Errorf("ConsoleSize requires a TTY and dimensions between 1 and 65535"))
			return
		}
		session.Config.ConsoleSize = append([]uint(nil), startConfig.ConsoleSize...)
	}
	if session.Config.AttachStdin && !startConfig.Detach && !upgrade {
		session.mu.Unlock()
		writeError(writer, http.StatusBadRequest, fmt.Errorf("exec AttachStdin requires an HTTP Upgrade: tcp connection"))
		return
	}

	var connection io.ReadWriteCloser
	var hijackedReader io.Reader
	if upgrade {
		hijacker, supported := writer.(http.Hijacker)
		if !supported {
			session.mu.Unlock()
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("HTTP server does not support exec stream hijacking"))
			return
		}
		conn, buffered, hijackErr := hijacker.Hijack()
		if hijackErr != nil {
			session.mu.Unlock()
			writeError(writer, http.StatusInternalServerError, fmt.Errorf("hijack Docker exec stream: %w", hijackErr))
			return
		}
		if _, writeErr := buffered.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); writeErr != nil {
			session.mu.Unlock()
			_ = conn.Close()
			return
		}
		if flushErr := buffered.Flush(); flushErr != nil {
			session.mu.Unlock()
			_ = conn.Close()
			return
		}
		connection = conn
		hijackedReader = buffered.Reader
	}

	session.Started = true
	session.Running = true
	started := time.Now().UTC()
	session.StartedAt = &started
	detach := startConfig.Detach
	attachStdin := session.Config.AttachStdin
	attachStdout := session.Config.AttachStdout
	attachStderr := session.Config.AttachStderr
	tty := session.Config.Tty
	session.mu.Unlock()

	if detach {
		writer.WriteHeader(http.StatusOK)
		go s.runDockerExecSession(context.Background(), session, nil, io.Discard, io.Discard)
		return
	}

	if connection != nil {
		defer connection.Close()
		var stdin io.Reader
		if attachStdin {
			stdin = hijackedReader
		}
		var stdout, stderr io.Writer = io.Discard, io.Discard
		var outputMu sync.Mutex
		if tty {
			if attachStdout || attachStderr {
				combined := &dockerExecTTYWriter{writer: connection, mu: &outputMu}
				stdout, stderr = combined, combined
			}
		} else {
			if attachStdout {
				stdout = &dockerExecFrameWriter{writer: connection, stream: 1, mu: &outputMu}
			}
			if attachStderr {
				stderr = &dockerExecFrameWriter{writer: connection, stream: 2, mu: &outputMu}
			}
		}
		s.runDockerExecSession(context.Background(), session, stdin, stdout, stderr)
		return
	}

	writer.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	var outputMu sync.Mutex
	var stdout io.Writer = io.Discard
	var stderr io.Writer = io.Discard
	if tty {
		if attachStdout || attachStderr {
			combined := &dockerExecTTYWriter{writer: writer, flusher: writerFlusher(writer), mu: &outputMu}
			stdout, stderr = combined, combined
		}
	} else {
		if attachStdout {
			stdout = &dockerExecFrameWriter{writer: writer, flusher: writerFlusher(writer), stream: 1, mu: &outputMu}
		}
		if attachStderr {
			stderr = &dockerExecFrameWriter{writer: writer, flusher: writerFlusher(writer), stream: 2, mu: &outputMu}
		}
	}
	s.runDockerExecSession(request.Context(), session, nil, stdout, stderr)
}

func writerFlusher(writer http.ResponseWriter) http.Flusher {
	flusher, _ := writer.(http.Flusher)
	return flusher
}

func (s *Server) runDockerExecSession(ctx context.Context, session *dockerExecSession, stdin io.Reader, stdout, stderr io.Writer) {
	exitCode, err := dockerExecRunner(ctx, session.ContainerID, session.Config, stdin, stdout, stderr, func(pid int) {
		session.mu.Lock()
		session.PID = pid
		session.mu.Unlock()
	}, func(terminal *os.File) {
		session.mu.Lock()
		session.terminal = terminal
		session.mu.Unlock()
	})
	if err != nil {
		exitCode = 125
		if stderr != nil && stderr != io.Discard {
			_, _ = fmt.Fprintf(stderr, "boxr exec: %v\n", err)
		}
	}
	if exitCode < 0 || exitCode > 255 {
		exitCode = 125
	}
	finished := time.Now().UTC()
	session.mu.Lock()
	session.Running = false
	session.terminal = nil
	session.ExitCode = &exitCode
	session.FinishedAt = &finished
	session.mu.Unlock()
}

func (s *Server) handleExecResize(writer http.ResponseWriter, request *http.Request, id string) {
	query := request.URL.Query()
	for key := range query {
		if key != "h" && key != "w" {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("unsupported exec resize query parameter %q", key))
			return
		}
	}
	heightValues, widthValues := query["h"], query["w"]
	if len(heightValues) != 1 || len(widthValues) != 1 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("exec resize requires exactly one h and one w parameter"))
		return
	}
	height, heightErr := strconv.ParseUint(heightValues[0], 10, 16)
	width, widthErr := strconv.ParseUint(widthValues[0], 10, 16)
	if heightErr != nil || widthErr != nil || height == 0 || width == 0 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("exec resize dimensions must be integers between 1 and 65535"))
		return
	}
	session, ok := s.dockerExecSession(id)
	if !ok {
		writeError(writer, http.StatusNotFound, fmt.Errorf("exec session %q not found", id))
		return
	}
	session.mu.Lock()
	if session.Removed {
		session.mu.Unlock()
		writeError(writer, http.StatusNotFound, fmt.Errorf("exec session %q not found", id))
		return
	}
	if !session.Config.Tty {
		session.mu.Unlock()
		writeError(writer, http.StatusBadRequest, fmt.Errorf("exec session was not created with a TTY"))
		return
	}
	if !session.Running || session.terminal == nil {
		session.mu.Unlock()
		writeError(writer, http.StatusConflict, fmt.Errorf("exec session does not have an active terminal"))
		return
	}
	err := resizeDockerExecTTY(session.terminal, uint16(height), uint16(width))
	session.mu.Unlock()
	if err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("resize exec terminal: %w", err))
		return
	}
	writer.WriteHeader(http.StatusOK)
}

func (s *Server) handleExecInspect(writer http.ResponseWriter, id string) {
	session, ok := s.dockerExecSession(id)
	if !ok {
		writeError(writer, http.StatusNotFound, fmt.Errorf("exec session %q not found", id))
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.Removed {
		writeError(writer, http.StatusNotFound, fmt.Errorf("exec session %q not found", id))
		return
	}
	entrypoint, arguments := "", []string{}
	if len(session.Config.Cmd) > 0 {
		entrypoint = session.Config.Cmd[0]
		arguments = append(arguments, session.Config.Cmd[1:]...)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"ID":       id,
		"Running":  session.Running,
		"ExitCode": session.ExitCode,
		"ProcessConfig": map[string]any{
			"privileged":  false,
			"user":        session.Config.User,
			"tty":         session.Config.Tty,
			"entrypoint":  entrypoint,
			"arguments":   arguments,
			"environment": append([]string(nil), session.Config.Env...),
			"workingDir":  session.Config.WorkingDir,
		},
		"OpenStdin":   session.Config.AttachStdin,
		"OpenStderr":  session.Config.AttachStderr,
		"OpenStdout":  session.Config.AttachStdout,
		"CanRemove":   !session.Running,
		"ContainerID": session.ContainerID,
		"Pid":         session.PID,
		"DetachKeys":  session.Config.DetachKeys,
	})
}

type dockerExecTTYWriter struct {
	writer  io.Writer
	flusher http.Flusher
	mu      *sync.Mutex
}

func (writer *dockerExecTTYWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := writeDockerExecBytes(writer.writer, data); err != nil {
		return 0, err
	}
	if writer.flusher != nil {
		writer.flusher.Flush()
	}
	return len(data), nil
}

type dockerExecFrameWriter struct {
	writer  io.Writer
	flusher http.Flusher
	stream  byte
	mu      *sync.Mutex
}

func (writer *dockerExecFrameWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	written := 0
	for len(data) > 0 {
		chunk := len(data)
		if uint64(chunk) > uint64(^uint32(0)) {
			chunk = int(^uint32(0))
		}
		writer.mu.Lock()
		var header [8]byte
		header[0] = writer.stream
		binary.BigEndian.PutUint32(header[4:], uint32(chunk))
		if err := writeDockerExecBytes(writer.writer, header[:]); err != nil {
			writer.mu.Unlock()
			return written, err
		}
		if err := writeDockerExecBytes(writer.writer, data[:chunk]); err != nil {
			writer.mu.Unlock()
			return written, err
		}
		if writer.flusher != nil {
			writer.flusher.Flush()
		}
		writer.mu.Unlock()
		written += chunk
		data = data[chunk:]
	}
	return written, nil
}

func writeDockerExecBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		data = data[count:]
	}
	return nil
}
