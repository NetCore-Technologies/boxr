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
				Readiness: &ReadinessProbe{TCPAddress: "127.0.0.1:5432", Attempts: 2},
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
	wantGroups := [][]string{{"database"}, {"web", "worker"}, {"proxy"}}
	if !reflect.DeepEqual(plan.StartGroups, wantGroups) {
		t.Fatalf("parallel start groups = %v, want %v", plan.StartGroups, wantGroups)
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
	if plan.Services[1].ReadinessType != "tcp" {
		t.Fatalf("readiness type = %q, want tcp", plan.Services[1].ReadinessType)
	}
	if strings.Contains(string(encoded), "127.0.0.1") {
		t.Fatalf("deployment plan leaked the private readiness target: %s", encoded)
	}
}

func TestBuildPlanRejectsInvalidGraphsAndServices(t *testing.T) {
	validService := Service{Command: []string{"/bin/true"}}
	tests := map[string]File{
		"unsupported version":  {Version: 2, Services: map[string]Service{"web": validService}},
		"empty services":       {Version: 1},
		"invalid stack name":   {Version: 1, Name: "../bad", Services: map[string]Service{"web": validService}},
		"invalid service name": {Version: 1, Services: map[string]Service{"Web": validService}},
		"missing command":      {Version: 1, Services: map[string]Service{"web": {}}},
		"unknown dependency": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, DependsOn: []string{"db"}},
		}},
		"self dependency": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, DependsOn: []string{"web"}},
		}},
		"duplicate dependency": {Version: 1, Services: map[string]Service{
			"db":  validService,
			"web": {Command: []string{"/bin/true"}, DependsOn: []string{"db", "db"}},
		}},
		"cycle": {Version: 1, Services: map[string]Service{
			"api": {Command: []string{"/bin/true"}, DependsOn: []string{"db"}},
			"db":  {Command: []string{"/bin/true"}, DependsOn: []string{"api"}},
		}},
		"invalid options": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, Options: container.Options{NetworkMode: "bridge"}},
		}},
		"non-loopback readiness URL": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, Readiness: &ReadinessProbe{HTTPURL: "http://example.com/health"}},
		}},
		"isolated network readiness": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, Options: container.Options{NetworkMode: "none"}, Readiness: &ReadinessProbe{TCPAddress: "127.0.0.1:8080"}},
		}},
		"restricted readiness profile": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, Options: container.Options{SecurityProfile: "restricted"}, Readiness: &ReadinessProbe{TCPAddress: "127.0.0.1:8080"}},
		}},
		"untrusted readiness profile": {Version: 1, Services: map[string]Service{
			"web": {Command: []string{"/bin/true"}, Options: container.Options{SecurityProfile: "untrusted", RootFS: "/tmp/boxr-test-rootfs"}, Readiness: &ReadinessProbe{TCPAddress: "127.0.0.1:8080"}},
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

func TestBuildPlanSupportsLocalImageServicesAndRejectsReservedLabels(t *testing.T) {
	file := File{
		Version: 1,
		Name:    "sample-stack",
		Services: map[string]Service{
			"web": {
				Image: "ghcr.io/example/web:1.2.3",
				Options: container.Options{
					NetworkMode: "none",
				},
			},
		},
	}
	plan, err := BuildPlan(file)
	if err != nil {
		t.Fatalf("BuildPlan(image service): %v", err)
	}
	if plan.Services[0].Image != file.Services["web"].Image {
		t.Fatalf("image reference = %q, want %q", plan.Services[0].Image, file.Services["web"].Image)
	}
	if plan.Services[0].ContainerName != ContainerName("sample-stack", "web") {
		t.Fatalf("container name = %q", plan.Services[0].ContainerName)
	}

	file.Services["web"] = Service{
		Image: "alpine:latest",
		Options: container.Options{
			Labels: map[string]string{StackLabel: "spoofed"},
		},
	}
	if _, err := BuildPlan(file); err == nil {
		t.Fatal("reserved deployment labels unexpectedly accepted")
	}
}

func TestBuildPlanValidatesNamedVolumeMounts(t *testing.T) {
	valid := File{
		Version: 1,
		Name:    "volume-stack",
		Services: map[string]Service{
			"database": {
				Command: []string{"/bin/database"},
				Options: container.Options{RootFS: "/srv/boxr/database"},
				Volumes: []VolumeMount{
					{Name: "db-data", Target: "/var/lib/database"},
					{Name: "db-backup", Target: "/var/backups", ReadOnly: true},
				},
			},
		},
	}
	plan, err := BuildPlan(valid)
	if err != nil {
		t.Fatalf("valid named volumes rejected: %v", err)
	}
	if len(plan.Services[0].Volumes) != 2 || plan.Services[0].Volumes[1].Name != "db-backup" || !plan.Services[0].Volumes[1].ReadOnly {
		t.Fatalf("named volumes missing from plan: %+v", plan.Services[0].Volumes)
	}
	cases := map[string]File{
		"missing rootfs": {Version: 1, Services: map[string]Service{
			"database": {Command: []string{"/bin/database"}, Volumes: []VolumeMount{{Name: "db-data", Target: "/var/lib/database"}}},
		}},
		"invalid name": {Version: 1, Services: map[string]Service{
			"database": {Command: []string{"/bin/database"}, Options: container.Options{RootFS: "/srv/root"}, Volumes: []VolumeMount{{Name: "../escape", Target: "/data"}}},
		}},
		"relative target": {Version: 1, Services: map[string]Service{
			"database": {Command: []string{"/bin/database"}, Options: container.Options{RootFS: "/srv/root"}, Volumes: []VolumeMount{{Name: "db-data", Target: "data"}}},
		}},
		"root target": {Version: 1, Services: map[string]Service{
			"database": {Command: []string{"/bin/database"}, Options: container.Options{RootFS: "/srv/root"}, Volumes: []VolumeMount{{Name: "db-data", Target: "/"}}},
		}},
		"path traversal": {Version: 1, Services: map[string]Service{
			"database": {Command: []string{"/bin/database"}, Options: container.Options{RootFS: "/srv/root"}, Volumes: []VolumeMount{{Name: "db-data", Target: "/data/../root"}}},
		}},
		"duplicate targets": {Version: 1, Services: map[string]Service{
			"database": {Command: []string{"/bin/database"}, Options: container.Options{RootFS: "/srv/root"}, Volumes: []VolumeMount{{Name: "db-data", Target: "/data"}, {Name: "db-backup", Target: "/data"}}},
		}},
		"bind target conflict": {Version: 1, Services: map[string]Service{
			"database": {
				Command: []string{"/bin/database"},
				Options: container.Options{RootFS: "/srv/root", BindMounts: []container.BindMount{{Source: "/host/config", Target: "/data"}}},
				Volumes: []VolumeMount{{Name: "db-data", Target: "/data"}},
			},
		}},
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildPlan(file); err == nil {
				t.Fatal("invalid named-volume manifest unexpectedly validated")
			}
		})
	}
}

func TestContainerNameIsStableAndBounded(t *testing.T) {
	stack := "deployment-stack-with-a-long-name"
	service := "service-name-that-is-also-very-long-and-near-the-limit"
	first := ContainerName(stack, service)
	second := ContainerName(stack, service)
	if first != second {
		t.Fatalf("container name is not deterministic: %q != %q", first, second)
	}
	if len(first) > 64 {
		t.Fatalf("container name has %d characters, maximum is 64", len(first))
	}
	if !strings.Contains(first, "-") {
		t.Fatalf("container name is not readable: %q", first)
	}
}

func TestBuildPlanRejectsImageAndRootFSTogether(t *testing.T) {
	_, err := BuildPlan(File{
		Version: 1,
		Name:    "sample",
		Services: map[string]Service{
			"web": {
				Image: "alpine:latest",
				Options: container.Options{
					RootFS: "/tmp/rootfs",
				},
			},
		},
	})
	if err == nil {
		t.Fatal("image and explicit rootfs unexpectedly accepted together")
	}
}

func TestBuildPlanProfilesSelectServicesAndKeepBaseServices(t *testing.T) {
	file := File{
		Version: 1,
		Name:    "profile-stack",
		Services: map[string]Service{
			"database": {Command: []string{"/bin/database"}},
			"api": {
				Command:   []string{"/bin/api"},
				DependsOn: []string{"database"},
				Profiles:  []string{"frontend"},
			},
			"worker": {
				Command:  []string{"/bin/worker"},
				Profiles: []string{"background", "diagnostics"},
			},
			"debug": {
				Command:  []string{"/bin/debug"},
				Profiles: []string{"diagnostics"},
			},
		},
	}

	defaultPlan, err := BuildPlan(file)
	if err != nil {
		t.Fatalf("BuildPlan() without profiles: %v", err)
	}
	if len(defaultPlan.Services) != 1 || defaultPlan.Services[0].Name != "database" {
		t.Fatalf("default active services = %+v, want only the unprofiled database", defaultPlan.Services)
	}
	if len(defaultPlan.Profiles) != 0 {
		t.Fatalf("default selected profiles = %v, want none", defaultPlan.Profiles)
	}

	plan, err := BuildPlanWithProfiles(file, []string{"diagnostics", "frontend"})
	if err != nil {
		t.Fatalf("BuildPlanWithProfiles(): %v", err)
	}
	wantProfiles := []string{"diagnostics", "frontend"}
	if !reflect.DeepEqual(plan.Profiles, wantProfiles) {
		t.Fatalf("selected profiles = %v, want %v", plan.Profiles, wantProfiles)
	}
	got := make(map[string]ServicePlan, len(plan.Services))
	for _, service := range plan.Services {
		got[service.Name] = service
	}
	for _, name := range []string{"database", "api", "worker", "debug"} {
		if _, exists := got[name]; !exists {
			t.Errorf("selected plan omitted service %q", name)
		}
	}
	if !reflect.DeepEqual(got["api"].Profiles, []string{"frontend"}) {
		t.Fatalf("api profile metadata = %v, want [frontend]", got["api"].Profiles)
	}
	if !reflect.DeepEqual(got["worker"].Profiles, []string{"background", "diagnostics"}) {
		t.Fatalf("worker profile metadata = %v", got["worker"].Profiles)
	}
}

func TestBuildPlanProfilesRejectInvalidSelectorsAndInactiveDependencies(t *testing.T) {
	base := File{
		Version: 1,
		Services: map[string]Service{
			"database": {
				Command:  []string{"/bin/database"},
				Profiles: []string{"storage"},
			},
			"api": {
				Command:   []string{"/bin/api"},
				DependsOn: []string{"database"},
				Profiles:  []string{"frontend"},
			},
		},
	}
	tests := map[string]struct {
		file     File
		profiles []string
	}{
		"unknown profile": {
			file:     base,
			profiles: []string{"missing"},
		},
		"duplicate selector": {
			file:     base,
			profiles: []string{"frontend", "frontend"},
		},
		"invalid selector": {
			file:     base,
			profiles: []string{"../frontend"},
		},
		"inactive dependency": {
			file:     base,
			profiles: []string{"frontend"},
		},
		"duplicate service profile": {
			file: File{Version: 1, Services: map[string]Service{
				"api": {Command: []string{"/bin/api"}, Profiles: []string{"frontend", "frontend"}},
			}},
			profiles: []string{"frontend"},
		},
		"invalid service profile": {
			file: File{Version: 1, Services: map[string]Service{
				"api": {Command: []string{"/bin/api"}, Profiles: []string{"../frontend"}},
			}},
			profiles: []string{"frontend"},
		},
		"no active services": {
			file: File{Version: 1, Services: map[string]Service{
				"api": {Command: []string{"/bin/api"}, Profiles: []string{"frontend"}},
			}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := BuildPlanWithProfiles(test.file, test.profiles)
			if err == nil {
				t.Fatal("invalid profile selection unexpectedly produced a plan")
			}
		})
	}
}
