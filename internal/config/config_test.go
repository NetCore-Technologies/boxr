package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValidVersionedConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "boxr.json")
	contents := `{"version":1,"name":"demo","command":["/bin/sh","-c","echo ok"],"options":{"hostname":"demo","networkMode":"none","memoryLimitBytes":268435456,"pidsLimit":64,"noNewPrivileges":true}}`
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(file)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if loaded.Version != 1 || loaded.Name != "demo" || len(loaded.Command) != 3 || loaded.Options.NetworkMode != "none" || loaded.Options.MemoryLimitBytes != 268435456 || loaded.Options.PidsLimit != 64 {
		t.Fatalf("unexpected config: %#v", loaded)
	}
}

func TestLoadRejectsInvalidConfigFiles(t *testing.T) {
	tests := map[string]string{
		"unsupported version": `{"version":2,"options":{}}`,
		"unknown field":       `{"version":1,"options":{"notARealOption":true}}`,
		"invalid network":     `{"version":1,"options":{"networkMode":"bridge"}}`,
		"relative rootfs":     `{"version":1,"options":{"rootfs":"rootfs"}}`,
		"empty command arg":   `{"version":1,"command":[""],"options":{}}`,
		"trailing json":       `{"version":1,"options":{}} {}`,
		"invalid json":        `{`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "invalid.json")
			if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(file); err == nil {
				t.Fatal("invalid config unexpectedly loaded")
			}
		})
	}
}

func TestLoadEnforcesConfigSizeLimit(t *testing.T) {
	file := filepath.Join(t.TempDir(), "large.json")
	contents := `{"version":1,"options":{},"padding":"` + strings.Repeat("x", MaxConfigBytes) + `"}`
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil {
		t.Fatal("oversized config unexpectedly loaded")
	}
}

func TestExampleIsLoadable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "example.json")
	if err := os.WriteFile(file, []byte(Example()), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err != nil {
		t.Fatalf("generated example is invalid: %v", err)
	}
}
