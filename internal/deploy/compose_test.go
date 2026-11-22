package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
    stop_signal: SIGQUIT
    stop_grace_period: 30s
    user: "1000:1000"
    init: true
    mem_limit: 512m
    cpus: 1.5
    pids_limit: 256
    mem_reservation: 128m
    cpu_shares: 1024
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
	if api.Options.MemoryLimitBytes != 512<<20 || api.Options.CPUs != 1.5 || api.Options.PidsLimit != 256 {
		t.Fatalf("resource limits = memory %d, CPUs %g, PIDs %d", api.Options.MemoryLimitBytes, api.Options.CPUs, api.Options.PidsLimit)
	}
	if api.Options.MemoryLowBytes != 128<<20 || api.Options.CPUWeight != 39 {
		t.Fatalf("resource reservations = memory.low %d, cpu.weight %d", api.Options.MemoryLowBytes, api.Options.CPUWeight)
	}
	if api.Options.User != "1000:1000" {
		t.Fatalf("container user = %q, want 1000:1000", api.Options.User)
	}
	if api.Options.StopSignal != "SIGQUIT" || api.StopTimeoutMS != 30000 {
		t.Fatalf("stop settings = signal %q grace %dms", api.Options.StopSignal, api.StopTimeoutMS)
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
		"interactive tty":                  "services:\n  api:\n    image: alpine\n    network_mode: none\n    tty: true\n",
		"privileged":                       "services:\n  api:\n    image: alpine\n    network_mode: none\n    privileged: true\n",
		"restart policy":                   "services:\n  api:\n    image: alpine\n    network_mode: none\n    restart: always\n",
		"invalid stop signal":              "services:\n  api:\n    image: alpine\n    network_mode: none\n    stop_signal: SIGKILL\n",
		"unknown stop signal":              "services:\n  api:\n    image: alpine\n    network_mode: none\n    stop_signal: SIGMAGIC\n",
		"invalid stop grace period":        "services:\n  api:\n    image: alpine\n    network_mode: none\n    stop_grace_period: 0s\n",
		"long stop grace period":           "services:\n  api:\n    image: alpine\n    network_mode: none\n    stop_grace_period: 10m1s\n",
		"implicit default bridge":          "services:\n  api:\n    image: alpine\n",
		"non-external network":             "services:\n  api:\n    image: alpine\n    networks: [app]\nnetworks:\n  app: {}\n",
		"unsupported dependency condition": "services:\n  api:\n    image: alpine\n    network_mode: none\n    depends_on:\n      db:\n        condition: service_completed_successfully\n  db:\n    image: postgres\n    network_mode: none\n",
		"invalid user syntax":               "services:\\n  api:\\n    image: alpine\\n    network_mode: none\\n    user: \"bad/user\"\\n",
		"invalid user whitespace":           "services:\\n  api:\\n    image: alpine\\n    network_mode: none\\n    user: \"1000: 1000\"\\n",
		"init disabled":                    "services:\\n  api:\\n    image: alpine\\n    network_mode: none\\n    init: false\\n",
		"invalid user delimiter":            "services:\\n  api:\\n    image: alpine\\n    network_mode: none\\n    user: \"1000:1000:1000\"\\n",
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
		"non-list":           "NET_RAW",
		"unknown capability": "[NOT_A_LINUX_CAPABILITY]",
		"empty capability":   "[NET_RAW, '']",
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

func TestLoadComposeTranslatesLongFormBindVolumeAndTmpfsMounts(t *testing.T) {
	dir := t.TempDir()
	bindDir := filepath.Join(dir, "config")
	if err := os.Mkdir(bindDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yaml")
	contents := strings.Join([]string{
		"services:",
		"  api:",
		"    image: alpine:3.21",
		"    network_mode: none",
		"    volumes:",
		"      - type: bind",
		"        source: ./config",
		"        target: /etc/app",
		"        read_only: true",
		"        bind:",
		"          propagation: rprivate",
		"          create_host_path: false",
		"      - type: volume",
		"        source: data",
		"        target: /var/lib/data",
		"        read_only: true",
		"        volume:",
		"          nocopy: true",
		"      - type: tmpfs",
		"        target: /run",
		"        tmpfs:",
		"          size: 64m",
		"          mode: 1023",
		"volumes:",
		"  data:",
		"    external: true",
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load Compose long-form mounts: %v", err)
	}
	service := file.Services["api"]
	if !reflect.DeepEqual(service.Volumes, []VolumeMount{{Name: "data", Target: "/var/lib/data", ReadOnly: true}}) {
		t.Fatalf("long-form external volume translation = %#v", service.Volumes)
	}
	if !reflect.DeepEqual(service.Options.BindMounts, []container.BindMount{{Source: bindDir, Target: "/etc/app", ReadOnly: true}}) {
		t.Fatalf("long-form bind translation = %#v", service.Options.BindMounts)
	}
	if !reflect.DeepEqual(service.Options.TmpfsMounts, []container.TmpfsMount{{Target: "/run", SizeBytes: 64 << 20, Mode: 1023}}) {
		t.Fatalf("long-form tmpfs translation = %#v", service.Options.TmpfsMounts)
	}
	if _, err := BuildPlan(file); err != nil {
		t.Fatalf("BuildPlan(long-form mounts): %v", err)
	}
}

func TestLoadComposeLongFormMountsRejectUnsupportedSemantics(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		mounts   string
		topLevel string
	}{
		{
			name:   "host path auto creation",
			mounts: "      - type: bind\n        source: ./config\n        target: /data\n        bind:\n          create_host_path: true",
		},
		{
			name:   "unsupported propagation",
			mounts: "      - type: bind\n        source: ./config\n        target: /data\n        bind:\n          propagation: rshared",
		},
		{
			name:   "unknown bind option",
			mounts: "      - type: bind\n        source: ./config\n        target: /data\n        bind:\n          recursive: disabled",
		},
		{
			name:     "volume copy up",
			mounts:   "      - type: volume\n        source: data\n        target: /data",
			topLevel: "volumes:\n  data:\n    external: true\n",
		},
		{
			name:     "volume copy up explicitly enabled",
			mounts:   "      - type: volume\n        source: data\n        target: /data\n        volume:\n          nocopy: false",
			topLevel: "volumes:\n  data:\n    external: true\n",
		},
		{
			name:     "volume subpath",
			mounts:   "      - type: volume\n        source: data\n        target: /data\n        volume:\n          nocopy: true\n          subpath: child",
			topLevel: "volumes:\n  data:\n    external: true\n",
		},
		{
			name:   "tmpfs missing size",
			mounts: "      - type: tmpfs\n        target: /run\n        tmpfs:\n          mode: 1023",
		},
		{
			name:   "tmpfs too large",
			mounts: "      - type: tmpfs\n        target: /run\n        tmpfs:\n          size: 2g",
		},
		{
			name:   "combined tmpfs limit",
			mounts: "      - type: tmpfs\n        target: /run/a\n        tmpfs:\n          size: 1g\n      - type: tmpfs\n        target: /run/b\n        tmpfs:\n          size: 1g\n      - type: tmpfs\n        target: /run/c\n        tmpfs:\n          size: 1g",
		},
		{
			name:   "tmpfs unknown option",
			mounts: "      - type: tmpfs\n        target: /run\n        tmpfs:\n          size: 64m\n          noexec: true",
		},
		{
			name:   "tmpfs marked read only",
			mounts: "      - type: tmpfs\n        target: /run\n        read_only: true\n        tmpfs:\n          size: 64m",
		},
		{
			name:   "target traversal",
			mounts: "      - type: tmpfs\n        target: /data/../escape\n        tmpfs:\n          size: 64m",
		},
		{
			name:   "duplicate mount target",
			mounts: "      - type: bind\n        source: ./config\n        target: /shared\n      - type: tmpfs\n        target: /shared\n        tmpfs:\n          size: 64m",
		},
		{
			name:   "nested mount targets",
			mounts: "      - type: bind\n        source: ./config\n        target: /workspace\n      - type: tmpfs\n        target: /workspace/cache\n        tmpfs:\n          size: 64m",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			contents := "services:\n  api:\n    image: alpine:3.21\n    network_mode: none\n    volumes:\n" + test.mounts + "\n" + test.topLevel
			path := filepath.Join(dir, strings.ReplaceAll(test.name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("unsupported long-form mount semantics were accepted")
			}
		})
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
			name:     "long-form volume requires explicit no-copy",
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

func TestLoadComposeInterpolatesEnvironmentAndDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	t.Setenv("BOXR_COMPOSE_IMAGE_REPO", "host.example/team/api")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("BOXR_COMPOSE_IMAGE_REPO=file.example/api\nBOXR_COMPOSE_IMAGE_TAG=1.2.3\nBOXR_COMPOSE_EMPTY=\nBOXR_COMPOSE_MESSAGE='hello # world'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	contents := `services:
  api:
    image: "${BOXR_COMPOSE_IMAGE_REPO}:${BOXR_COMPOSE_IMAGE_TAG}"
    command: ["sh", "-c", "echo $$HOME ${BOXR_COMPOSE_MESSAGE:-fallback}"]
    environment:
      EMPTY_DEFAULT: "${BOXR_COMPOSE_EMPTY:-fallback}"
      INHERITED: "${BOXR_COMPOSE_IMAGE_REPO}"
      REQUIRED: "${BOXR_COMPOSE_IMAGE_TAG:?image tag is required}"
      LITERAL: '${BOXR_COMPOSE_NOT_EXPANDED}'
      OPTIONAL: "${BOXR_COMPOSE_NOT_SET+present}"
    network_mode: none
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load Compose YAML: %v", err)
	}
	api := file.Services["api"]
	if api.Image != "host.example/team/api:1.2.3" {
		t.Fatalf("image = %q; host environment must override project .env", api.Image)
	}
	if !reflect.DeepEqual(api.Command, []string{"sh", "-c", "echo $HOME hello # world"}) {
		t.Fatalf("command = %#v", api.Command)
	}
	for key, want := range map[string]string{
		"EMPTY_DEFAULT": "fallback",
		"INHERITED":     "host.example/team/api",
		"REQUIRED":      "1.2.3",
		"LITERAL":       "${BOXR_COMPOSE_NOT_EXPANDED}",
	} {
		if !containsComposeEnv(api.Options.Env, key, want) {
			t.Errorf("environment %s=%q missing from %#v", key, want, api.Options.Env)
		}
	}
	if containsComposeEnv(api.Options.Env, "OPTIONAL", "present") {
		t.Fatalf("unset + alternate unexpectedly produced a value: %#v", api.Options.Env)
	}
}

func TestComposeInterpolationOperatorsAndNestedDefaults(t *testing.T) {
	environment := map[string]string{
		"SET":    "value",
		"EMPTY":  "",
		"NUMBER": "42",
	}
	cases := []struct {
		input string
		want  string
	}{
		{"$SET", "value"},
		{"${SET}", "value"},
		{"${MISSING:-default}", "default"},
		{"${MISSING-nonempty}", "nonempty"},
		{"${EMPTY:-fallback}", "fallback"},
		{"${EMPTY-empty-default}", ""},
		{"${SET:-fallback}", "value"},
		{"${SET+alternate}", "alternate"},
		{"${EMPTY+alternate}", "alternate"},
		{"${EMPTY:+alternate}", ""},
		{"${SET:+alternate}", "alternate"},
		{"${MISSING:-${UNSET_NUMBER:-nested}}", "nested"},
		{"$$SET", "$SET"},
		{"prefix-$NUMBER-suffix", "prefix-42-suffix"},
		{"literal $ punctuation", "literal $ punctuation"},
	}
	for _, test := range cases {
		got, err := expandComposeInterpolation(test.input, environment, 0)
		if err != nil {
			t.Errorf("expand %q: %v", test.input, err)
			continue
		}
		if got != test.want {
			t.Errorf("expand %q = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestComposeInterpolationRejectsUndefinedMalformedAndRequiredValues(t *testing.T) {
	for _, input := range []string{
		"${MISSING}",
		"${EMPTY:?set a non-empty value}",
		"${MISSING?set a value}",
		"${}",
		"${MISSING",
		"${SET:unsupported}",
	} {
		if _, err := expandComposeInterpolation(input, map[string]string{"EMPTY": "", "SET": "value"}, 0); err == nil {
			t.Errorf("expand %q unexpectedly succeeded", input)
		}
	}
}

func TestComposeInterpolationDoesNotRewriteYAMLKeysOrSingleQuotedScalars(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte("services:\n  api:\n    image: 'literal:$IMAGE'\n    environment:\n      $KEY: value\n      VALUE: \"$IMAGE\"\n"), &root); err != nil {
		t.Fatal(err)
	}
	if err := interpolateComposeNode(&root, map[string]string{"IMAGE": "expanded", "KEY": "renamed"}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(&root)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, "literal:$IMAGE") || !strings.Contains(text, "$KEY: value") || !strings.Contains(text, `VALUE: "expanded"`) {
		t.Fatalf("interpolation changed literal values or keys unexpectedly:\n%s", text)
	}
}

func TestComposeDotEnvParserBoundsAndQuotedValues(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"plain value # comment", "plain value"},
		{"value#kept", "value#kept"},
		{"'literal $VALUE # value'", "literal $VALUE # value"},
		{"\"line\\nvalue\"", "line\nvalue"},
		{"\"quote: \\\"yes\\\"\"", "quote: \"yes\""},
	}
	for _, test := range cases {
		got, err := parseComposeDotEnvValue(test.input)
		if err != nil || got != test.want {
			t.Errorf("parseComposeDotEnvValue(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	if _, err := parseComposeDotEnvValue("'unterminated"); err == nil {
		t.Fatal("unterminated single quote was accepted")
	}
}

func TestLoadComposeInterpolationRejectsSymlinkedDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	target := filepath.Join(dir, "secrets")
	if err := os.WriteFile(target, []byte("IMAGE=alpine:3.21\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".env")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(path, []byte("services:\n  api:\n    image: alpine:3.21\n    network_mode: none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "regular non-symlink") {
		t.Fatalf("Load with symlinked .env error = %v", err)
	}
}

func TestLoadComposeTranslatesDockerHealthchecks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	contents := strings.Join([]string{
		"services:",
		"  api:",
		"    image: alpine:3.21",
		"    network_mode: none",
		"    healthcheck:",
		"      test: [\"CMD\", \"/usr/local/bin/health\", \"--ready\"]",
		"      interval: 45s",
		"      timeout: 3s",
		"      start_period: 20s",
		"      start_interval: 2s",
		"      retries: 5",
		"  worker:",
		"    image: busybox:1.36",
		"    network_mode: none",
		"    healthcheck:",
		"      test: \"test -f /run/ready\"",
		"      retries: 2",
		"  inherited:",
		"    image: alpine:3.21",
		"    network_mode: none",
		"    healthcheck:",
		"      test: [\"NONE\"]",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load Compose healthchecks: %v", err)
	}
	api := file.Services["api"].Options.Healthcheck
	if api == nil || !reflect.DeepEqual(api.Test, []string{"CMD", "/usr/local/bin/health", "--ready"}) ||
		api.Interval != 45*time.Second || api.Timeout != 3*time.Second ||
		api.StartPeriod != 20*time.Second || api.StartInterval != 2*time.Second || api.Retries != 5 {
		t.Fatalf("api healthcheck = %#v", api)
	}
	worker := file.Services["worker"].Options.Healthcheck
	if worker == nil || !reflect.DeepEqual(worker.Test, []string{"CMD-SHELL", "test -f /run/ready"}) ||
		worker.Retries != 2 || worker.Interval != container.DefaultHealthcheckInterval {
		t.Fatalf("worker healthcheck = %#v", worker)
	}
	inherited := file.Services["inherited"].Options.Healthcheck
	if inherited == nil || !reflect.DeepEqual(inherited.Test, []string{"NONE"}) {
		t.Fatalf("inherited healthcheck override = %#v", inherited)
	}
	if _, err := BuildPlan(file); err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
}

func TestComposeHealthcheckRejectsInvalidOrUnsupportedOptions(t *testing.T) {
	cases := map[string]string{
		"unknown field":         "    healthcheck:\n      test: [CMD, /bin/check]\n      retries: 3\n      shell: /bin/sh\n",
		"missing test":          "    healthcheck:\n      retries: 3\n",
		"invalid retries":       "    healthcheck:\n      test: [CMD, /bin/check]\n      retries: 0\n",
		"invalid interval":      "    healthcheck:\n      test: [CMD, /bin/check]\n      interval: 0s\n",
		"invalid timeout":       "    healthcheck:\n      test: [CMD, /bin/check]\n      timeout: invalid\n",
		"empty command":         "    healthcheck:\n      test: [CMD, '']\n",
		"too many command args": "    healthcheck:\n      test: [CMD, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17]\n",
		"disable with test":     "    healthcheck:\n      disable: true\n      test: [CMD, /bin/check]\n",
		"disable with timing":   "    healthcheck:\n      disable: true\n      interval: 10s\n",
		"malformed test":        "    healthcheck:\n      test: [SHELL, /bin/check]\n",
	}
	for name, healthcheck := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compose.yaml")
			contents := "services:\n  api:\n    image: alpine:3.21\n    network_mode: none\n" + healthcheck
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid healthcheck configuration was accepted")
			}
		})
	}
}

func TestComposeHealthcheckDisableFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	contents := "services:\n  api:\n    image: alpine:3.21\n    network_mode: none\n    healthcheck:\n      disable: true\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := file.Services["api"].Options.Healthcheck
	if got == nil || !reflect.DeepEqual(got.Test, []string{"NONE"}) {
		t.Fatalf("healthcheck disable = %#v", got)
	}
}

func TestLoadComposeSupportsServiceHealthyDependency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	contents := strings.Join([]string{
		"services:",
		"  database:",
		"    image: postgres:16",
		"    network_mode: none",
		"    healthcheck:",
		"      test: [\"CMD\", \"pg_isready\"]",
		"      interval: 1s",
		"  api:",
		"    image: example/api:1.0",
		"    network_mode: none",
		"    depends_on:",
		"      database:",
		"        condition: service_healthy",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load Compose service_healthy dependency: %v", err)
	}
	api := file.Services["api"]
	if !reflect.DeepEqual(api.DependsOn, []string{"database"}) ||
		!reflect.DeepEqual(api.DependsOnHealthy, []string{"database"}) {
		t.Fatalf("api dependencies = %#v / %#v", api.DependsOn, api.DependsOnHealthy)
	}
	plan, err := BuildPlan(file)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !reflect.DeepEqual(plan.StartGroups, [][]string{{"database"}, {"api"}}) {
		t.Fatalf("start groups = %#v", plan.StartGroups)
	}
	if !reflect.DeepEqual(plan.Services[1].DependsOnHealthy, []string{"database"}) {
		t.Fatalf("planned healthy dependencies = %#v", plan.Services[1].DependsOnHealthy)
	}
}

func TestLoadComposeRejectsHealthyDependencyWithoutEnabledHealthcheck(t *testing.T) {
	for name, healthcheck := range map[string]string{
		"missing":  "",
		"disabled": "    healthcheck:\n      test: [\"NONE\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compose.yaml")
			contents := "services:\n  database:\n    image: postgres:16\n    network_mode: none\n" + healthcheck
			contents += "  api:\n    image: example/api:1.0\n    network_mode: none\n    depends_on:\n      database:\n        condition: service_healthy\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := Load(path)
			if err != nil {
				t.Fatalf("Load Compose manifest: %v", err)
			}
			if _, err := BuildPlan(file); err == nil {
				t.Fatal("service_healthy dependency without an enabled healthcheck was accepted by BuildPlan")
			}
		})
	}
}

func TestParseComposeStopGracePeriod(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"100ms", 100},
		{"10.5ms", 11},
		{"30s", 30000},
		{"10m", 600000},
	}
	for _, test := range cases {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: test.input}
		got, err := parseComposeStopGracePeriod(node)
		if err != nil || got != test.want {
			t.Errorf("parseComposeStopGracePeriod(%q) = %d, %v; want %d", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"", "9ms", "0s", "-1s", "10m1s", "bad"} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: input}
		if _, err := parseComposeStopGracePeriod(node); err == nil {
			t.Errorf("parseComposeStopGracePeriod(%q) unexpectedly succeeded", input)
		}
	}
	if _, err := parseComposeStopGracePeriod(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "30"}); err == nil {
		t.Fatal("integer stop_grace_period unexpectedly succeeded")
	}
}

func TestParseComposeStopSignal(t *testing.T) {
	for _, input := range []string{"", "SIGTERM", "TERM", "SIGINT", "SIGQUIT", "SIGHUP", "SIGUSR1", "SIGUSR2"} {
		if got, err := parseComposeStopSignal(input); err != nil || got == "" {
			t.Errorf("parseComposeStopSignal(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"SIGKILL", "SIGSTOP", "SIGCONT", "SIGMAGIC"} {
		if _, err := parseComposeStopSignal(input); err == nil {
			t.Errorf("parseComposeStopSignal(%q) unexpectedly succeeded", input)
		}
	}
}


func TestParseComposeUser(t *testing.T) {
	cases := []string{"1000", "1000:1000", "root", "app", ":1000"}
	for _, input := range cases {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: input}
		got, err := parseComposeUser(node)
		if err != nil || got != input {
			t.Errorf("parseComposeUser(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "bad/user", "1000: 1000", "1000:1000:1000", "app\nroot"} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: input}
		if _, err := parseComposeUser(node); err == nil {
			t.Errorf("parseComposeUser(%q) unexpectedly succeeded", input)
		}
	}
	if _, err := parseComposeUser(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}); err == nil {
		t.Fatal("boolean Compose user unexpectedly succeeded")
	}
}


func TestParseComposeResourceLimits(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
	}{
		{"512m", 512 << 20},
		{"1GiB", 1 << 30},
		{"4096", 4096},
		{"8kb", 8 << 10},
	} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: test.value}
		got, err := parseComposeMemoryLimit(node)
		if err != nil || got != test.want {
			t.Errorf("parseComposeMemoryLimit(%q) = %d, %v; want %d", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"", "0", "-1", "512.5m", "999999999999999999999999g"} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		if _, err := parseComposeMemoryLimit(node); err == nil {
			t.Errorf("parseComposeMemoryLimit(%q) unexpectedly succeeded", value)
		}
	}
}

func TestParseComposeCPUs(t *testing.T) {
	for _, test := range []struct {
		value string
		want  float64
	}{
		{"0.5", 0.5},
		{"1", 1},
		{"1.75", 1.75},
	} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: test.value}
		got, err := parseComposeCPUs(node)
		if err != nil || got != test.want {
			t.Errorf("parseComposeCPUs(%q) = %g, %v; want %g", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"0", "-1", "NaN", "+Inf", "0.000001", "1000001"} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		if _, err := parseComposeCPUs(node); err == nil {
			t.Errorf("parseComposeCPUs(%q) unexpectedly succeeded", value)
		}
	}
}

func TestParseComposeMemoryReservation(t *testing.T) {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "128m"}
	got, err := parseComposeMemoryReservation(node)
	if err != nil || got != 128<<20 {
		t.Fatalf("parseComposeMemoryReservation() = %d, %v", got, err)
	}
	if _, err := parseComposeMemoryReservation(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "0"}); err == nil {
		t.Fatal("zero Compose mem_reservation unexpectedly succeeded")
	}
}

func TestParseComposeCPUShares(t *testing.T) {
	for _, test := range []struct {
		shares string
		weight int
	}{
		{"2", 1},
		{"1024", 39},
		{"262144", 10000},
	} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: test.shares}
		got, err := parseComposeCPUShares(node)
		if err != nil || got != test.weight {
			t.Errorf("parseComposeCPUShares(%s) = %d, %v; want %d", test.shares, got, err, test.weight)
		}
	}
	for _, value := range []string{"1", "262145", "1.5", "-1"} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		if _, err := parseComposeCPUShares(node); err == nil {
			t.Errorf("parseComposeCPUShares(%q) unexpectedly succeeded", value)
		}
	}
}

func TestParseComposePidsLimit(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
	}{
		{"1", 1},
		{"256", 256},
		{"-1", 0},
	} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: test.value}
		got, err := parseComposePidsLimit(node)
		if err != nil || got != test.want {
			t.Errorf("parseComposePidsLimit(%q) = %d, %v; want %d", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"0", "-2", "1.5", "9223372036854775808"} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		if _, err := parseComposePidsLimit(node); err == nil {
			t.Errorf("parseComposePidsLimit(%q) unexpectedly succeeded", value)
		}
	}
}

func TestParseComposeInit(t *testing.T) {
	if err := parseComposeInit(nil); err != nil {
		t.Fatalf("unset init should use Boxr's built-in init: %v", err)
	}
	for _, value := range []string{"true", "TRUE"} {
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value}
		if err := parseComposeInit(node); err != nil {
			t.Errorf("parseComposeInit(%q): %v", value, err)
		}
	}
	for _, node := range []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "true"},
		{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"},
		{Kind: yaml.SequenceNode},
	} {
		if err := parseComposeInit(node); err == nil {
			t.Errorf("parseComposeInit(%#v) unexpectedly succeeded", node)
		}
	}
}
