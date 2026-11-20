package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
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
