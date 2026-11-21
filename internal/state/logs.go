package state

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxLogBytes int64 = 64 << 20

// OpenLog opens the private append-only output log for an existing container.
// The file is always created with owner-only permissions.
func OpenLog(query string) (*os.File, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	record, err := findLocked(query)
	if err != nil {
		return nil, err
	}
	dir, err := ensureDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, record.ID+".log")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("container log must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open container log: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure container log: %w", err)
	}
	return file, nil
}

// LogPath returns the log path for an existing container. The path is intended
// for read-only streaming by the CLI; callers must not replace or truncate it.
func LogPath(query string) (string, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	record, err := findLocked(query)
	if err != nil {
		return "", err
	}
	dir, err := ensureDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, record.ID+".log"), nil
}

// ReadLogs returns all log bytes when tail is zero, or only the last tail lines
// when tail is positive. Logs are bounded to avoid untrusted output exhausting
// the CLI process memory.
func ReadLogs(query string, tail int) ([]byte, error) {
	data, _, err := ReadLogsSnapshot(query, tail)
	return data, err
}

// openLogRead resolves an existing container log and verifies the opened
// descriptor is still the same regular file that was inspected with Lstat.
func openLogRead(query string) (*os.File, os.FileInfo, error) {
	path, err := LogPath(query)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("container log must be a regular file")
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open container log: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("inspect opened container log: %w", err)
		}
		return nil, nil, fmt.Errorf("container log changed while opening")
	}
	return file, openedInfo, nil
}

// LogOffset returns the current end-of-file offset without loading log contents.
// Attach clients use it to follow only new output when logs=false.
func LogOffset(query string) (int64, error) {
	file, info, err := openLogRead(query)
	if err != nil || file == nil {
		return 0, err
	}
	defer file.Close()
	return info.Size(), nil
}

// ReadLogsSnapshot reads a bounded snapshot and returns the byte offset at the
// end of that exact snapshot. It lets --follow start without losing output that
// is appended between an initial tail read and opening the follow stream.
func ReadLogsSnapshot(query string, tail int) ([]byte, int64, error) {
	if tail < 0 {
		return nil, 0, fmt.Errorf("tail line count cannot be negative")
	}
	file, info, err := openLogRead(query)
	if err != nil {
		return nil, 0, err
	}
	if file == nil {
		return []byte{}, 0, nil
	}
	defer file.Close()
	if info.Size() > maxLogBytes {
		return nil, 0, fmt.Errorf("container log exceeds %d bytes; rotate or remove it before reading", maxLogBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLogBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read container log: %w", err)
	}
	if int64(len(data)) > maxLogBytes {
		return nil, 0, fmt.Errorf("container log exceeds %d bytes", maxLogBytes)
	}
	offset := int64(len(data))
	if tail == 0 || len(data) == 0 {
		return data, offset, nil
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= tail {
		return data, offset, nil
	}
	return bytes.Join(lines[len(lines)-tail:], nil), offset, nil
}

// CopyLogFrom streams new bytes from offset to dst. It returns the new offset;
// each call copies at most maxLogBytes so a large or unbounded log cannot force
// a follow stream to buffer the entire file in memory.
func CopyLogFrom(query string, offset int64, dst io.Writer) (int64, error) {
	if offset < 0 {
		return offset, fmt.Errorf("log offset cannot be negative")
	}
	file, info, err := openLogRead(query)
	if err != nil || file == nil {
		return offset, err
	}
	defer file.Close()
	if offset > info.Size() {
		offset = 0 // log was rotated or truncated
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	remaining := info.Size() - offset
	if remaining <= 0 {
		return offset, nil
	}
	if remaining > maxLogBytes {
		remaining = maxLogBytes
	}
	written, err := io.CopyN(dst, file, remaining)
	if err != nil && err != io.EOF {
		return offset + written, err
	}
	return offset + written, nil
}

// ValidateLogQuery is a small helper used by commands that need to ensure the
// requested record exists before waiting for a log file to appear.
func ValidateLogQuery(query string) error {
	_, err := LogPath(strings.TrimSpace(query))
	return err
}
