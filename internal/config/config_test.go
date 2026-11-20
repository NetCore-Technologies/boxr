package config

import (
	"reflect"
	"encoding/json"
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

func TestLoadAcceptsManagedBridgeNetworkConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bridge.json")
	contents := `{"version":1,"name":"api","command":["/bin/sh"],"options":{"networkMode":"bridge","networkName":"frontend"}}`
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(file)
	if err != nil {
		t.Fatalf("Load(bridge config): %v", err)
	}
	if loaded.Options.NetworkMode != "bridge" || loaded.Options.NetworkName != "frontend" {
		t.Fatalf("loaded bridge options = %+v", loaded.Options)
	}
}

func TestLoadRejectsInvalidConfigFiles(t *testing.T) {
	tests := map[string]string{
		"unsupported version": `{"version":2,"options":{}}`,
		"unknown field":       `{"version":1,"options":{"notARealOption":true}}`,
		"bridge missing name":  `{"version":1,"options":{"networkMode":"bridge"}}`,
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

func TestJSONSchemaIsStrictAndDescribesSecurityProfiles(t *testing.T) {
	var schema struct {
		Schema               string                     `json:"$schema"`
		Type                 string                     `json:"type"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(JSONSchema()), &schema); err != nil {
		t.Fatalf("JSONSchema() returned invalid JSON: %v", err)
	}
	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatalf("root schema is not strict: %#v", schema)
	}
	for _, required := range []string{"version", "options"} {
		found := false
		for _, item := range schema.Required {
			if item == required {
				found = true
			}
		}
		if !found {
			t.Errorf("schema required list is missing %q", required)
		}
	}
	var options struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema.Properties["options"], &options); err != nil {
		t.Fatal(err)
	}
	if _, ok := options.Properties["securityProfile"]; !ok {
		t.Fatal("schema does not describe securityProfile")
	}
	var networkMode struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(options.Properties["networkMode"], &networkMode); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(networkMode.Enum, []string{"host", "none", "bridge"}) {
		t.Fatalf("networkMode schema enum = %v", networkMode.Enum)
	}
	if _, ok := options.Properties["networkName"]; !ok {
		t.Fatal("schema does not describe networkName")
	}
}
