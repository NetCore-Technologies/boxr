package state

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maxEngineConfigBytes = 1 << 20

// SaveEngineConfig stores sensitive Engine API create settings separately from
// container records. The payload is restricted to an existing created record.
func SaveEngineConfig(id string, data []byte) error {
	if len(data) == 0 || len(data) > maxEngineConfigBytes {
		return fmt.Errorf("Engine API configuration must be between 1 byte and %d bytes", maxEngineConfigBytes)
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	record, err := findLocked(id)
	if err != nil {
		return err
	}
	if record.Status != "created" {
		return fmt.Errorf("Engine API configuration can only be saved for a created container")
	}
	directory, err := engineConfigDirectoryLocked()
	if err != nil {
		return err
	}
	path := engineConfigPath(directory, record.ID)
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("Engine API configuration path is not a regular file")
		}
		return fmt.Errorf("Engine API configuration already exists for container %s", record.ID)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	file, err := os.CreateTemp(directory, ".engine-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create Engine API configuration: %w", err)
	}
	tempPath := file.Name()
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish Engine API configuration: %w", err)
	}
	committed = true
	return nil
}

// ReadEngineConfig returns the private create configuration for a container.
func ReadEngineConfig(id string) ([]byte, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	record, err := findLocked(id)
	if err != nil {
		return nil, err
	}
	directory, err := engineConfigDirectoryLocked()
	if err != nil {
		return nil, err
	}
	path := engineConfigPath(directory, record.ID)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() > maxEngineConfigBytes {
		return nil, fmt.Errorf("Engine API configuration is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("Engine API configuration changed while opening")
	}
	if after.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("Engine API configuration permissions must be owner-only")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxEngineConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEngineConfigBytes {
		return nil, fmt.Errorf("Engine API configuration exceeds %d bytes", maxEngineConfigBytes)
	}
	return data, nil
}

// EngineConfigPath returns the canonical sidecar path for an existing record.
func EngineConfigPath(id string) (string, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	record, err := findLocked(id)
	if err != nil {
		return "", err
	}
	directory, err := engineConfigDirectoryLocked()
	if err != nil {
		return "", err
	}
	return engineConfigPath(directory, record.ID), nil
}

func engineConfigDirectoryLocked() (string, error) {
	stateDirectory, err := ensureDir()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(stateDirectory, "engine-config")
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create Engine API configuration directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("Engine API configuration directory must be a private directory owned by the current user")
	}
	return directory, nil
}

func engineConfigPath(directory, id string) string {
	return filepath.Join(directory, id+".json")
}

func removeEngineConfigLocked(id string) error {
	stateDirectory, err := Dir()
	if err != nil {
		return err
	}
	directory := filepath.Join(stateDirectory, "engine-config")
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("Engine API configuration directory is not a real directory")
	}
	path := engineConfigPath(directory, id)
	entry, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if entry.Mode()&os.ModeSymlink != 0 || !entry.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove a non-regular Engine API configuration")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove Engine API configuration: %w", err)
	}
	return nil
}
