package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"gopkg.in/yaml.v3"
)

func TestLoadComposeYAMLTranslatesSupportedServices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	t.Setenv("BOXR_COMPOSE_INHERITED", "from-host")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("FROM_FILE=loaded\n"), 0600); err != nil {
		t.Fatal(err)
	}
	contents := `name: sample-stack
services:
  database:
    image: postgres:16
    command: ["postgres", "-c", "max_connections=200"]
    environment:
      POSTGRES_DB: app
    networks:
      app: {}
  api:
    image: ghcr.io/example/api:1.2.3
    command: /app/server --message "hello world"
    depends_on:
      database:
        condition: service_started
    environment:
      APP_MODE: production
      BOXR_COMPOSE_INHERITED:
    env_file: .env
    ports:
      - "127.0.0.1:8080:8080/tcp"
    labels:
      io.example.component: api
    profiles: [frontend]
    working_dir: /srv/app
    hostname: api
    read_only: true
    cap_drop: [NET_RAW, CAP_MKNOD]
    networks: [app]
networks:
  app:
    external: true
    name: boxr-app
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load Compose YAML: %v", err)
	}
	if file.Version != 1 || file.Name != "sample-stack" {
		t.Fatalf("deployment identity = version %d, name %q", file.Version, file.Name)
	}
	api := file.Services["api"]
	if !reflect.DeepEqual(api.Command, []string{"/app/server", "--message", "hello world"}) {
		t.Fatalf("command = %#v", api.Command)
	}
	if !reflect.DeepEqual(api.DependsOn, []string{"database"}) {
		t.Fatalf("depends_on = %#v", api.DependsOn)
	}
	if api.Options.NetworkMode != "bridge" || api.Options.NetworkName != "boxr-app" {
		t.Fatalf("network options = %+v", api.Options)
	}
	if len(api.Options.PublishedPorts) != 1 || api.Options.PublishedPorts[0] != (container.PortMapping{
		HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 8080, Protocol: "tcp",
	}) {
		t.Fatalf("published ports = %#v", api.Options.PublishedPorts)
	}
	if api.Options.Workdir != "/srv/app" || api.Options.Hostname != "api" {
		t.Fatalf("workdir/hostname = %q / %q", api.Options.Workdir, api.Options.Hostname)
	}
	if !api.Options.ReadOnlyRootFS {
		t.Fatal("Compose read_only was not translated to a read-only root filesystem")
	}
	if !reflect.DeepEqual(api.Options.DropCapabilities, []string{"NET_RAW", "MKNOD"}) {
		t.Fatalf("cap_drop = %#v, want [NET_RAW MKNOD]", api.Options.DropCapabilities)
	}
	if !containsComposeEnv(api.Options.Env, "APP_MODE", "production") ||
		!containsComposeEnv(api.Options.Env, "BOXR_COMPOSE_INHERITED", "from-host") {
		t.Fatalf("environment = %#v", api.Options.Env)
	}
	if !reflect.DeepEqual(api.EnvFiles, []string{filepath.Join(dir, ".env")}) {
		t.Fatalf("env files = %#v", api.EnvFiles)
	}
	if api.Options.Labels["io.example.component"] != "api" {
		t.Fatalf("labels = %#v", api.Options.Labels)
	}
	plan, err := BuildPlanWithProfiles(file, []string{"frontend"})
	if err != nil {
		t.Fatalf("BuildPlanWithProfiles: %v", err)
	}
	if !reflect.DeepEqual(plan.ExecutionOrder, []string{"database", "api"}) {
		t.Fatalf("execution order = %#v", plan.ExecutionOrder)
	}
}

func TestLoadComposeDerivesProjectNameAndAcceptsIsolatedServices(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Compose.Project")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte("services:\n  worker:\n    image: alpine:3.21\n    network_mode: none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Name != "my-compose-project" {
		t.Fatalf("derived project name = %q", file.Name)
	}
	if file.Services["worker"].Options.NetworkMode != "none" {
		t.Fatalf("network mode = %q", file.Services["worker"].Options.NetworkMode)
	}
	if _, err := BuildPlan(file); err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
}

func TestLoadComposeRejectsSemanticsBoxrCannotRepresent(t *testing.T) {
	tests := map[string]string{
		"image build":                      "services:\n  api:\n    build: .\n",
		"named volumes":                    "services:\n  api:\n    image: alpine\n    network_mode: none\n    volumes: [data:/data]\n",
		"top-level volumes":                "services:\n  api:\n    image: alpine\n    network_mode: none\nvolumes:\n  data: {}\n",
		"entrypoint override":              "services:\n  api:\n    image: alpine\n    network_mode: none\n    entrypoint: [/bin/sh]\n",
		"healthcheck command":              "services:\n  api:\n    image: alpine\n    network_mode: host\n    healthcheck:\n      test: [CMD, /bin/check]\n",
		"interactive tty":                  "services:\n  api:\n    image: alpine\n    network_mode: none\n    tty: true\n",
		"privileged":                       "services:\n  api:\n    image: alpine\n    network_mode: none\n    privileged: true\n",
		"restart policy":                   "services:\n  api:\n    image: alpine\n    network_mode: none\n    restart: always\n",
		"implicit default bridge":          "services:\n  api:\n    image: alpine\n",
		"non-external network":             "services:\n  api:\n    image: alpine\n    networks: [app]\nnetworks:\n  app: {}\n",
		"unsupported dependency condition": "services:\n  api:\n    image: alpine\n    network_mode: none\n    depends_on:\n      db:\n        condition: service_healthy\n  db:\n    image: postgres\n    network_mode: none\n",
		"unknown service field":            "services:\n  api:\n    image: alpine\n    network_mode: none\n    deploy: {}\n",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compose.yaml")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("unsupported Compose document was accepted")
			}
		})
	}
}


func TestLoadComposeValidatesCapabilityDrops(t *testing.T) {
	tests := map[string]string{
		"non-list": "NET_RAW",
		"unknown capability": "[NOT_A_LINUX_CAPABILITY]",
		"empty capability": "[NET_RAW, '']",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compose.yaml")
			contents := "services:\n  api:\n    image: alpine:3.21\n    network_mode: none\n    cap_drop: " + value + "\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid cap_drop configuration was accepted")
			}
		})
	}
}

func TestLoadComposeRejectsMalformedYAMLAndMultipleDocuments(t *testing.T) {
	for name, contents := range map[string]string{
		"malformed":          "services:\n  api: [\n",
		"duplicate key":      "services:\n  api:\n    image: alpine\n    image: busybox\n    network_mode: none\n",
		"multiple documents": "services:\n  api:\n    image: alpine\n    network_mode: none\n---\nservices:\n  worker:\n    image: busybox\n    network_mode: none\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compose.yaml")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid YAML unexpectedly loaded")
			}
		})
	}
}

func TestSplitComposeCommandHonoursQuotesWithoutShellExpansion(t *testing.T) {
	got, err := splitComposeCommand(`/app/server --title "hello there" 'two words' $HOME`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/app/server", "--title", "hello there", "two words", "$HOME"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command tokens = %#v, want %#v", got, want)
	}
	for _, invalid := range []string{`/bin/tool "unfinished`, "/bin/tool trailing\\", `/bin/tool ""`} {
		if _, err := splitComposeCommand(invalid); err == nil {
			t.Errorf("invalid command %q was accepted", invalid)
		}
	}
}

func TestLoadKeepsStrictJSONDeploymentManifests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	contents := `{"version":1,"name":"json-stack","services":{"worker":{"command":["/bin/true"],"options":{"networkMode":"none"}}}}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("strict JSON manifest failed to load: %v", err)
	}
	if file.Name != "json-stack" || !reflect.DeepEqual(file.Services["worker"].Command, []string{"/bin/true"}) {
		t.Fatalf("loaded JSON manifest = %+v", file)
	}
}

func containsComposeEnv(values []string, key, want string) bool {
	for _, value := range values {
		name, actual, ok := strings.Cut(value, "=")
		if ok && name == key && actual == want {
			return true
		}
	}
	return false
}

func TestLoadComposeTranslatesExternalVolumesAndExistingBindMounts(t *testing.T) {
	dir := t.TempDir()
	bindDir := filepath.Join(dir, "config")
	if err := os.Mkdir(bindDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindDir, "app.conf"), []byte("mode=production\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yaml")
	contents := `services:
  api:
    image: alpine:3.21
    network_mode: none
    volumes:
      - data:/var/lib/data:ro
      - ./config:/etc/app:ro
volumes:
  data:
    external:
      name: boxr-data
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load Compose with external volume and bind mount: %v", err)
	}
	service := file.Services["api"]
	if !reflect.DeepEqual(service.Volumes, []VolumeMount{{Name: "boxr-data", Target: "/var/lib/data", ReadOnly: true}}) {
		t.Fatalf("external volume mounts = %#v", service.Volumes)
	}
	if !reflect.DeepEqual(service.Options.BindMounts, []container.BindMount{{
		Source: bindDir, Target: "/etc/app", ReadOnly: true,
	}}) {
		t.Fatalf("host bind mounts = %#v", service.Options.BindMounts)
	}
	plan, err := BuildPlan(file)
	if err != nil {
		t.Fatalf("BuildPlan(image mounts): %v", err)
	}
	if len(plan.Services) != 1 || plan.Services[0].Name != "api" {
		t.Fatalf("service plan = %#v", plan.Services)
	}
}

func TestLoadComposeRejectsUnsafeOrImplicitVolumeSemantics(t *testing.T) {
	dir := t.TempDir()
	missingBind := filepath.Join(dir, "does-not-exist")
	cases := []struct {
		name     string
		volumes  string
		topLevel string
	}{
		{
			name:     "implicit named volume creation",
			volumes:  "volumes: [data:/data]",
			topLevel: "volumes:\n  data: {}\n",
		},
		{
			name:    "undeclared named volume",
			volumes: "volumes: [data:/data]",
		},
		{
			name:    "unsupported propagation mode",
			volumes: "volumes: [./config:/data:ro,z]",
		},
		{
			name:     "long syntax",
			volumes:  "volumes:\n      - type: volume\n        source: data\n        target: /data\n",
			topLevel: "volumes:\n  data:\n    external: true\n",
		},
		{
			name:    "missing bind source",
			volumes: "volumes: [" + missingBind + ":/data]",
		},
		{
			name:     "external volume mapping without name",
			volumes:  "volumes: [data:/data]",
			topLevel: "volumes:\n  data:\n    external: {}\n",
		},
		{
			name:     "external name conflicts with declared name",
			volumes:  "volumes: [data:/data]",
			topLevel: "volumes:\n  data:\n    name: boxr-data\n    external:\n      name: another-volume\n",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			network := "    network_mode: none\n"
			service := "    image: alpine:3.21\n" + network
			if test.volumes != "" {
				service += "    " + strings.ReplaceAll(test.volumes, "\n", "\n    ") + "\n"
			}
			contents := "services:\n  api:\n" + service + test.topLevel
			path := filepath.Join(t.TempDir(), "compose.yaml")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("unsupported volume configuration was accepted")
			}
		})
	}
}

func TestParseComposeExternalVolumesRejectsInvalidBoxrNames(t *testing.T) {
	_, err := parseComposeExternalVolumes(map[string]composeVolume{
		"data": {External: yaml.Node{Kind: yaml.ScalarNode, Value: "true"}, Name: "../host"},
	})
	if err == nil {
		t.Fatal("invalid external Boxr volume name was accepted")
	}
}
