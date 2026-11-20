package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
)

func TestBuildPlanOrdersServicesDeterministicallyAndRedactsSecrets(t *testing.T) {
	file := File{
		Version: 1,
		Name:    "sample",
		Services: map[string]Service{
			"web": {
				Command:   []string{"/bin/sh", "-c", "echo private-command-token"},
				Options:   container.Options{Env: []string{"TOKEN=private-environment-token"}, Labels: map[string]string{"owner": "private-label"}},
				DependsOn: []string{"database"},
			},
			"worker": {
				Command:   []string{"/bin/worker"},
				Options:   container.Options{NetworkMode: "none", PidsLimit: 64},
				DependsOn: []string{"database"},
			},
			"database": {
				Command: []string{"/bin/database"},
				Options: container.Options{SecurityProfile: "restricted", MemoryLimitBytes: 1024},
			},
			"proxy": {
				Command:   []string{"/bin/proxy"},
				Options:   container.Options{},
				DependsOn: []string{"worker", "web"},
			},
		},
	}
	plan, err := BuildPlan(file)
	if err != nil {
		t.Fatalf("BuildPlan(): %v", err)
	}
	wantOrder := []string{"database", "web", "worker", "proxy"}
	if !reflect.DeepEqual(plan.ExecutionOrder, wantOrder) {
		t.Fatalf("execution order = %v, want %v", plan.ExecutionOrder, wantOrder)
	}
	again, err := BuildPlan(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, again) {
		t.Fatal("deployment plan is not deterministic")
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-command-token", "private-environment-token", "private-label"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("deployment plan leaked %q: %s", secret, encoded)
		}
	}
	if plan.Services[1].CommandArgumentCount != 3 || plan.Services[1].EnvironmentEntryCount != 1 || plan.Services[1].LabelCount != 1 {
		t.Fatalf("redacted service summary lost useful counts: %+v", plan.Services[1])
	}
}

func TestBuildPlanRejectsInvalidGraphsAndServices(t *testing.T) {
	validService := Service{Command: []string{"/bin/true"}}
	tests := map[string]File{
		"unsupported version": {Version: 2, Services: map[string]Service{"web": validService}},
		"empty services":      {Version: 1},
		"invalid stack name":  {Version: 1, Name: "../bad", Services: map[string]Service{"web": validService}},
		"invalid service name": {Version: 1, Services: map[string]Service{"Web": validService}},
		"missing command":     {Version: 1, Services: map[string]Service{"web": {}}},
		"unknown dependency": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, DependsOn: []string{"db"}},
		}},
		"self dependency": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, DependsOn: []string{"web"}},
		}},
		"duplicate dependency": {Version: 1, Services: map[string]Service{
			"db": validService,
			"web": {Command: []string{"/bin/true"}, DependsOn: []string{"db", "db"}},
		}},
		"cycle": {Version: 1, Services: map[string]Service{
			"api": {Command: []string{"/bin/true"}, DependsOn: []string{"db"}},
			"db":  {Command: []string{"/bin/true"}, DependsOn: []string{"api"}},
		}},
		"invalid options": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, Options: container.Options{NetworkMode: "bridge"}},
		}},
		"duplicate container names": {Version: 1, Services: map[string]Service{
			"api": {Command: []string{"/bin/true"}, Options: container.Options{Name: "same-name"}},
			"web": {Command: []string{"/bin/true"}, Options: container.Options{Name: "same-name"}},
		}},
	}
	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildPlan(file); err == nil {
				t.Fatal("invalid deployment unexpectedly produced a plan")
			}
		})
	}
}

func TestLoadRejectsUnknownTrailingAndOversizedManifests(t *testing.T) {
	tests := map[string]string{
		"unknown field": `{"version":1,"services":{},"surprise":true}`,
		"trailing JSON": `{"version":1,"services":{"web":{"command":["/bin/true"]}}} {}`,
		"invalid JSON":  "{",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "deployment.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid manifest unexpectedly loaded")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "large.json")
	contents := `{"version":1,"services":{},"padding":"` + strings.Repeat("x", MaxManifestBytes) + `"}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("oversized manifest unexpectedly loaded")
	}
}

func TestExampleIsValidAndServiceLimitIsEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.json")
	if err := os.WriteFile(path, []byte(Example()), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load(Example()): %v", err)
	}
	if _, err := BuildPlan(file); err != nil {
		t.Fatalf("BuildPlan(Example()): %v", err)
	}
	services := make(map[string]Service, MaxServices+1)
	for index := 0; index < MaxServices+1; index++ {
		name := fmt.Sprintf("service-%02d", index)
		services[name] = Service{Command: []string{"/bin/true"}}
	}
	if _, err := BuildPlan(File{Version: 1, Services: services}); err == nil {
		t.Fatal("manifest with too many services unexpectedly validated")
	}
}
