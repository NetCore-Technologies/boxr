// Package config handles versioned Boxr run configuration files.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/manit6752025/boxr/internal/container"
)

const MaxConfigBytes = 1 << 20

// RunFile is the stable JSON format for reusable container run configurations.
type RunFile struct {
	Version           int               `json:"version"`
	Name              string            `json:"name,omitempty"`
	Command           []string          `json:"command,omitempty"`
	Entrypoint        *[]string         `json:"entrypoint,omitempty"`
	ClearImageCommand bool              `json:"clearImageCommand,omitempty"`
	Options           container.Options `json:"options"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

func Load(path string) (RunFile, error) {
	var result RunFile
	file, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil {
		return result, fmt.Errorf("read config: %w", err)
	}
	if len(data) > MaxConfigBytes {
		return result, fmt.Errorf("config exceeds %d bytes", MaxConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, fmt.Errorf("config must contain exactly one JSON value")
	}
	if result.Version != 1 {
		return result, fmt.Errorf("unsupported config version %d (supported: 1)", result.Version)
	}
	if strings.ContainsRune(result.Name, '\x00') {
		return result, fmt.Errorf("config name contains a NUL character")
	}
	for i, arg := range result.Command {
		if arg == "" || strings.ContainsRune(arg, '\x00') {
			return result, fmt.Errorf("command argument %d is empty or contains NUL", i)
		}
	}
	if result.Entrypoint != nil {
		for i, arg := range *result.Entrypoint {
			if arg == "" || strings.ContainsRune(arg, '\x00') {
				return result, fmt.Errorf("entrypoint argument %d is empty or contains NUL", i)
			}
		}
	}
	for key, value := range result.Metadata {
		if strings.TrimSpace(key) == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			return result, fmt.Errorf("metadata contains an empty key or NUL character")
		}
	}
	if err := result.Options.Validate(); err != nil {
		return result, fmt.Errorf("invalid options: %w", err)
	}
	return result, nil
}

func Example() string {
	return `{
  "version": 1,
  "name": "web-shell",
  "command": ["/bin/sh"],
  "options": {
    "hostname": "web",
    "rootfs": "/var/lib/boxr/rootfs/alpine",
    "readOnlyRootfs": true,
    "mountProc": true,
    "networkMode": "none",
    "userNamespace": true,
    "noNewPrivileges": true,
    "seccompProfile": "baseline",
    "dropCapabilities": ["NET_RAW", "SYS_ADMIN"],
    "memoryLimitBytes": 268435456,
    "memoryHighBytes": 201326592,
    "cpus": 1.0,
    "pidsLimit": 128,
    "tmpfsMounts": [{"target": "/tmp", "sizeBytes": 67108864, "mode": 1023}]
  },
  "metadata": {"purpose": "development shell"}
}
`
}
