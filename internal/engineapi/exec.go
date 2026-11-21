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
	"path"
	"strings"
	"sync"
	"time"

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
	Detach       bool
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
	DetachKeys  string
}

type dockerExecSession struct {
	ID          string
	ContainerID string
	Config      dockerExecCreateConfig
	CreatedAt   time.Time

	mu         sync.Mutex
	Started    bool
	Running    bool
	StartedAt  *time.Time
	FinishedAt *time.Time
	ExitCode   *int
	PID        int
}

type dockerExecRunnerFunc func(context.Context, string, dockerExecCreateConfig, io.Writer, io.Writer, func(int)) (int, error)

// dockerExecRunner uses Boxr's native exec command so PID-identity checks,
// namespace entry and cgroup attachment remain part of the execution path.
var dockerExecRunner dockerExecRunnerFunc = executeDockerExec

func executeDockerExec(ctx context.Context, containerID string, config dockerExecCreateConfig, stdout, stderr io.Writer, onStart func(int)) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 125, fmt.Errorf("resolve Boxr executable: %w", err)
	}
	arguments := []string{"exec"}
	if config.WorkingDir != "" {
		arguments = append(arguments, "--workdir", config.WorkingDir)
	}
	for _, value := range config.Env {
		arguments = append(arguments, "--env", value)
	}
	arguments = append(arguments, containerID)
	arguments = append(arguments, config.Cmd...)

	command := exec.CommandContext(ctx, executable, arguments...)
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
	if config.AttachStdin {
		return fmt.Errorf("AttachStdin is not supported yet; Docker exec stdin stream hijacking is unavailable")
	}
	if config.Tty {
		return fmt.Errorf("Tty is not supported yet; Docker exec PTY hijacking is unavailable")
	}
	if config.Privileged {
		return fmt.Errorf("Privileged exec is not supported")
	}
	if config.User != "" {
		return fmt.Errorf("per-exec User overrides are not supported; exec inherits the container's configured identity")
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
	if len(config.ConsoleSize) != 0 && len(config.ConsoleSize) != 2 {
		return fmt.Errorf("ConsoleSize must contain height and width")
	}
	if strings.ContainsAny(config.DetachKeys, "\x00\r\n") {
		return fmt.Errorf("DetachKeys cannot contain NUL or newlines")
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
	if config.Tty {
		return config, fmt.Errorf("Tty is not supported yet; Docker exec PTY hijacking is unavailable")
	}
	if len(config.ConsoleSize) != 0 && len(config.ConsoleSize) != 2 {
		return config, fmt.Errorf("ConsoleSize must contain height and width")
	}
	if strings.ContainsAny(config.DetachKeys, "\x00\r\n") {
		return config, fmt.Errorf("DetachKeys cannot contain NUL or newlines")
	}
	return config, nil
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
	session.mu.Lock()
	if session.Started {
		session.mu.Unlock()
		writeError(writer, http.StatusConflict, fmt.Errorf("exec session %q has already been started", id))
		return
	}
	session.Started = true
	session.Running = true
	started := time.Now().UTC()
	session.StartedAt = &started
	detach := session.Config.Detach || startConfig.Detach
	attachStdout := session.Config.AttachStdout
	attachStderr := session.Config.AttachStderr
	session.mu.Unlock()

	if detach {
		writer.WriteHeader(http.StatusOK)
		go s.runDockerExecSession(context.Background(), session, io.Discard, io.Discard)
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
	if attachStdout {
		stdout = &dockerExecFrameWriter{writer: writer, flusher: writerFlusher(writer), stream: 1, mu: &outputMu}
	}
	if attachStderr {
		stderr = &dockerExecFrameWriter{writer: writer, flusher: writerFlusher(writer), stream: 2, mu: &outputMu}
	}
	s.runDockerExecSession(context.Background(), session, stdout, stderr)
}

func writerFlusher(writer http.ResponseWriter) http.Flusher {
	flusher, _ := writer.(http.Flusher)
	return flusher
}

func (s *Server) runDockerExecSession(ctx context.Context, session *dockerExecSession, stdout, stderr io.Writer) {
	exitCode, err := dockerExecRunner(ctx, session.ContainerID, session.Config, stdout, stderr, func(pid int) {
		session.mu.Lock()
		session.PID = pid
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
	session.ExitCode = &exitCode
	session.FinishedAt = &finished
	session.mu.Unlock()
}

func (s *Server) handleExecInspect(writer http.ResponseWriter, id string) {
	session, ok := s.dockerExecSession(id)
	if !ok {
		writeError(writer, http.StatusNotFound, fmt.Errorf("exec session %q not found", id))
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
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
			"user":        "",
			"tty":         false,
			"entrypoint":  entrypoint,
			"arguments":   arguments,
			"environment": append([]string(nil), session.Config.Env...),
			"workingDir":  session.Config.WorkingDir,
		},
		"OpenStdin":   false,
		"OpenStderr":  session.Config.AttachStderr,
		"OpenStdout":  session.Config.AttachStdout,
		"CanRemove":   !session.Running,
		"ContainerID": session.ContainerID,
		"Pid":         session.PID,
	})
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
