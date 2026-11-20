package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/deploy"
	imageStore "github.com/manit6752025/boxr/internal/image"
	boxrnetwork "github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/rootfs"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/volume"
)

func TestImagePushCommandPublishesLocalImage(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "boxr-release"), []byte("local image for push test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var importOut, importErr bytes.Buffer
	if code := run([]string{"image", "import", root, "demo:latest"}, &importOut, &importErr); code != 0 {
		t.Fatalf("image import returned %d: %s", code, importErr.String())
	}
	uploaded := map[string]bool{}
	var manifestUploaded bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", "/v2/team/demo/blobs/uploads/session?upload=1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/blobs/uploads/session"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read pushed blob: %v", err)
			}
			digest := r.URL.Query().Get("digest")
			if got := testSHA256(body); got != digest {
				t.Errorf("uploaded blob digest = %q, want %q", got, digest)
			}
			uploaded[digest] = true
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/v1"):
			manifestUploaded = true
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/demo:v1"
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "push", "--authfile", authFile, "demo:latest", ref}, &out, &errOut); code != 0 {
		t.Fatalf("image push returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "pushed "+ref) || !manifestUploaded || len(uploaded) != 2 {
		t.Fatalf("incomplete image push: output=%q uploaded=%#v manifest=%v", out.String(), uploaded, manifestUploaded)
	}
}

func TestImagePushRequiresSource(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "push"}, &out, &errOut); code != 2 {
		t.Fatalf("image push without source returned %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Usage: boxr image push") {
		t.Fatalf("unexpected usage output: %q", errOut.String())
	}
}

func TestImagePullCommandImportsVerifiedBoxrRootFS(t *testing.T) {
	var tarData bytes.Buffer
	tarWriter := tar.NewWriter(&tarData)
	content := []byte("pulled from local test registry\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "etc/boxr-release", Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var layer bytes.Buffer
	gzipWriter := gzip.NewWriter(&layer)
	if _, err := gzipWriter.Write(tarData.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	layerDigest := testSHA256(layer.Bytes())
	diffSum := sha256.Sum256(tarData.Bytes())
	diffID := "sha256:" + hex.EncodeToString(diffSum[:])
	configData, err := json.Marshal(map[string]any{
		"os": "linux", "architecture": "arm64", "variant": "v8",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{diffID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	configDigest := testSHA256(configData)
	manifestData, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(configData)},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.boxr.rootfs.v1.tar+gzip", "digest": layerDigest, "size": layer.Len()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := testSHA256(manifestData)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestData)
		case "/v2/team/app/blobs/" + configDigest:
			_, _ = w.Write(configData)
		case "/v2/team/app/blobs/" + layerDigest:
			_, _ = w.Write(layer.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:v1"
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "pull", "--platform", "linux/arm64/v8", "--authfile", authFile, ref}, &out, &errOut); code != 0 {
		t.Fatalf("image pull returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "pulled "+strings.TrimPrefix(server.URL, "http://")+"/team/app:v1") {
		t.Fatalf("pull output does not show imported image: %q", out.String())
	}
	var listOut, listErr bytes.Buffer
	if code := run([]string{"image", "ls", "--quiet"}, &listOut, &listErr); code != 0 {
		t.Fatalf("image ls returned %d: %s", code, listErr.String())
	}
	if !strings.Contains(listOut.String(), "/team/app:v1") {
		t.Fatalf("pulled image missing from local store: %q", listOut.String())
	}
	var inspectOut, inspectErr bytes.Buffer
	if code := run([]string{"image", "inspect", strings.TrimPrefix(server.URL, "http://") + "/team/app:v1"}, &inspectOut, &inspectErr); code != 0 {
		t.Fatalf("image inspect returned %d: %s", code, inspectErr.String())
	}
	var stored struct {
		OS      string `json:"os"`
		Arch    string `json:"architecture"`
		Variant string `json:"variant"`
	}
	if err := json.Unmarshal(inspectOut.Bytes(), &stored); err != nil {
		t.Fatalf("decode imported image metadata: %v (%s)", err, inspectOut.String())
	}
	if stored.OS != "linux" || stored.Arch != "arm64" || stored.Variant != "v8" {
		t.Fatalf("cross-platform metadata was not preserved: %#v", stored)
	}
}

func TestImagePullRequiresReference(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "pull"}, &out, &errOut); code != 2 {
		t.Fatalf("image pull without reference returned %d, want 2: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "Usage: boxr image pull") {
		t.Fatalf("unexpected usage error: %q", errOut.String())
	}
}

func testSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestRunHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"help"}, &out, &errOut); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "boxr run") {
		t.Fatalf("help output does not contain run usage: %q", out.String())
	}
}

func TestRunVersion(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	t.Cleanup(func() { version, commit, buildDate = oldVersion, oldCommit, oldBuildDate })
	version, commit, buildDate = "v1.2.3", "abc123", "2026-11-18"

	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, errOut.String())
	}
	for _, want := range []string{"v1.2.3", "abc123", "2026-11-18"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("version output %q does not contain %q", out.String(), want)
		}
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"not-a-command"}, &out, &errOut); code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("unexpected error output: %q", errOut.String())
	}
}

func TestRunRequiresContainerCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"run"}, &out, &errOut); code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "a command is required") {
		t.Fatalf("unexpected error output: %q", errOut.String())
	}
}

func TestRunValidatesMemoryBeforeStartingContainer(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--memory", "0", "/bin/true"}, &out, &errOut); code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "invalid --memory") {
		t.Fatalf("unexpected error output: %q", errOut.String())
	}
}

func TestParseMemoryLimit(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"1024", 1024},
		{"1b", 1},
		{"1k", 1 << 10},
		{"2KiB", 2 << 10},
		{"256m", 256 << 20},
		{"1GiB", 1 << 30},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseMemoryLimit(test.input)
			if err != nil {
				t.Fatalf("parseMemoryLimit(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("parseMemoryLimit(%q) = %d, want %d", test.input, got, test.want)
			}
		})
	}
}

func TestParseMemoryLimitRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"", "0", "-1", "1.5g", "999999999999999999999g", "12tb"} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseMemoryLimit(input); err == nil {
				t.Fatalf("parseMemoryLimit(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestValidateEnv(t *testing.T) {
	for _, input := range []string{"A=1", "APP_ENV=dev", "_PRIVATE=value", "VALUE_WITH_DIGIT2=x"} {
		if err := validateEnv(input); err != nil {
			t.Errorf("validateEnv(%q): %v", input, err)
		}
	}
	for _, input := range []string{"", "A", "=value", "1BAD=value", "BAD-NAME=value", "A=bad\x00value"} {
		if err := validateEnv(input); err == nil {
			t.Errorf("validateEnv(%q) unexpectedly succeeded", input)
		}
	}
}

func TestRunDoctorReportsReadOnlyHostChecks(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"doctor"}, &out, &errOut)
	if code < 0 || code > 1 {
		t.Fatalf("run(doctor) code = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "boxr doctor") {
		t.Fatalf("doctor output is missing its heading: %q", out.String())
	}
	if !strings.Contains(out.String(), "Security boundary") {
		t.Fatalf("doctor output is missing the security warning: %q", out.String())
	}
}

func TestRunRejectsInvalidRuntimeOptionsBeforeStarting(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "negative cpu", args: []string{"run", "--cpus", "-1", "/bin/true"}, want: "--cpus"},
		{name: "nan cpu", args: []string{"run", "--cpus", "NaN", "/bin/true"}, want: "--cpus"},
		{name: "invalid hostname", args: []string{"run", "--hostname", strings.Repeat("h", 65), "/bin/true"}, want: "--hostname"},
		{name: "invalid environment", args: []string{"run", "--env", "BAD-NAME=value", "/bin/true"}, want: "--env"},
		{name: "nul workdir", args: []string{"run", "--workdir", "bad\x00path", "/bin/true"}, want: "--workdir"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(test.args, &out, &errOut); code != 2 {
				t.Fatalf("run() code = %d, want 2; stderr = %q", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), test.want) {
				t.Fatalf("stderr %q does not contain %q", errOut.String(), test.want)
			}
		})
	}
}

func TestParseBindMount(t *testing.T) {
	mount, err := parseBindMount("/var/data:/data:ro")
	if err != nil {
		t.Fatalf("parseBindMount(): %v", err)
	}
	if mount.Source != "/var/data" || mount.Target != "/data" || !mount.ReadOnly {
		t.Fatalf("unexpected mount: %#v", mount)
	}
	mount, err = parseBindMount("/tmp:/tmp")
	if err != nil || mount.ReadOnly {
		t.Fatalf("rw default mount = %#v, %v", mount, err)
	}
	for _, input := range []string{"", "relative:/data", "/src:relative", "/src:/dst:bad", "/src:/dst:ro:extra"} {
		if _, err := parseBindMount(input); err == nil {
			t.Errorf("parseBindMount(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseTmpfsMount(t *testing.T) {
	mount, err := parseTmpfsMount("/tmp:64m:1777")
	if err != nil {
		t.Fatalf("parseTmpfsMount(): %v", err)
	}
	if mount.Target != "/tmp" || mount.SizeBytes != 64<<20 || mount.Mode != 01777 {
		t.Fatalf("unexpected tmpfs: %#v", mount)
	}
	mount, err = parseTmpfsMount("/run")
	if err != nil || mount.Mode != 01777 || mount.SizeBytes != 0 {
		t.Fatalf("tmpfs defaults = %#v, %v", mount, err)
	}
	for _, input := range []string{"", "relative", "/tmp:0", "/tmp:64m:99999", "/tmp:64m:bad", "/tmp:64m:1777:extra"} {
		if _, err := parseTmpfsMount(input); err == nil {
			t.Errorf("parseTmpfsMount(%q) unexpectedly succeeded", input)
		}
	}
}

func TestRunRejectsInvalidRootfsAndNamespaceOptionsBeforeExecution(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--read-only-rootfs", "/bin/true"},
		{"run", "--rootfs", "relative", "/bin/true"},
		{"run", "--network", "bridge", "/bin/true"},
		{"run", "--cap-drop", "NOT_REAL", "/bin/true"},
		{"run", "--rootfs", "/tmp/rootfs", "--workdir", "relative", "/bin/true"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stderr=%q)", args, code, errOut.String())
		}
	}
}

func TestReadEnvFileParsesCommentsExportsAndQuotes(t *testing.T) {
	file := t.TempDir() + "/env"
	contents := "# comment\n\nA=one\nexport B=\"two words\"\nC='three words'\nHASH=a#b\n"
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readEnvFile(file)
	if err != nil {
		t.Fatalf("readEnvFile(): %v", err)
	}
	want := []string{"A=one", "B=two words", "C=three words", "HASH=a#b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readEnvFile() = %#v, want %#v", got, want)
	}
}

func TestReadEnvFileRejectsMalformedEntries(t *testing.T) {
	file := t.TempDir() + "/bad-env"
	for _, contents := range []string{"BAD-NAME=value\n", "NO_EQUALS\n", "=empty\n", "A=bad\x00value\n"} {
		if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readEnvFile(file); err == nil {
			t.Errorf("readEnvFile(%q) unexpectedly succeeded", contents)
		}
	}
}

func TestMergeEnvValuesUsesLastValueAcrossSources(t *testing.T) {
	got := mergeEnvValues([]string{"A=config", "B=config"}, []string{"A=file", "C=file"}, []string{"A=flag"})
	want := []string{"A=flag", "B=config", "C=file"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvValues() = %#v, want %#v", got, want)
	}
}

func TestConfigExampleCommandAndCompletion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"config", "example"}, &out, &errOut); code != 0 {
		t.Fatalf("config example: %d %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"version": 1`) {
		t.Fatalf("unexpected example: %q", out.String())
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		out.Reset()
		errOut.Reset()
		if code := run([]string{"completion", shell}, &out, &errOut); code != 0 || out.Len() == 0 {
			t.Errorf("completion %s: code=%d out=%q err=%q", shell, code, out.String(), errOut.String())
		}
	}
}

func TestDeployExamplePlanAndValidationCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	contents := `{
		"version": 1,
		"name": "test-stack",
		"services": {
			"api": {
				"command": ["/bin/sh", "-c", "echo private-command"],
				"options": {"networkMode": "none", "env": ["TOKEN=private-env"]},
				"dependsOn": ["database"]
			},
			"database": {
				"command": ["/bin/true"],
				"options": {},
				"readiness": {
					"httpUrl": "http://127.0.0.1:8081/healthz",
					"attempts": 1
				}
			}
		}
	}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "validate", path}, &out, &errOut); code != 0 {
		t.Fatalf("deploy validate: code=%d err=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "database -> api") {
		t.Fatalf("validate output lacks dependency order: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "plan", "--json", path}, &out, &errOut); code != 0 {
		t.Fatalf("deploy plan --json: code=%d err=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"executionOrder"`) || !strings.Contains(out.String(), `"readinessType": "http"`) ||
		strings.Contains(out.String(), "127.0.0.1") || strings.Contains(out.String(), "private-command") || strings.Contains(out.String(), "private-env") {
		t.Fatalf("JSON plan is missing ordering/readiness metadata or leaked private data: %q", out.String())
	}
	var plan struct {
		ExecutionOrder []string `json:"executionOrder"`
	}
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil || !reflect.DeepEqual(plan.ExecutionOrder, []string{"database", "api"}) {
		t.Fatalf("invalid plan JSON: %#v, error=%v", plan, err)
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "plan", path}, &out, &errOut); code != 0 {
		t.Fatalf("deploy plan: code=%d err=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "READINESS") || !strings.Contains(out.String(), "http") {
		t.Fatalf("text plan omitted readiness metadata: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "example"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy example: code=%d err=%q", code, errOut.String())
	}
	example := filepath.Join(t.TempDir(), "example.json")
	if err := os.WriteFile(example, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"deploy", "validate", example}, &out, &errOut); code != 0 {
		t.Fatalf("generated deployment example did not validate: %q", errOut.String())
	}
}

func TestDeployStatusDryRunDownAndRemoval(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	create := func(service, name string, order int) state.Record {
		t.Helper()
		labels := map[string]string{
			deploy.StackLabel:   "test-stack",
			deploy.ServiceLabel: service,
			deploy.OrderLabel:   strconv.Itoa(order),
		}
		record, err := state.Create(name, []string{"/bin/true", "private-command"}, container.Options{
			Env: []string{"TOKEN=private-deployment-secret"},
		}, labels)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	database := create("database", deploy.ContainerName("test-stack", "database"), 0)
	api := create("api", deploy.ContainerName("test-stack", "api"), 1)
	other, err := state.Create("other-stack-api", []string{"/bin/true"}, container.Options{}, map[string]string{
		deploy.StackLabel:   "other-stack",
		deploy.ServiceLabel: "api",
		deploy.OrderLabel:   "0",
	})
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "ps", "--json", "test-stack"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy ps: code=%d err=%q", code, errOut.String())
	}
	var statuses []deploymentStatus
	if err := json.Unmarshal(out.Bytes(), &statuses); err != nil {
		t.Fatalf("decode deploy ps output: %v", err)
	}
	if len(statuses) != 2 || statuses[0].Service != "database" || statuses[1].Service != "api" {
		t.Fatalf("unexpected deployment statuses: %#v", statuses)
	}
	if strings.Contains(out.String(), "private-command") || strings.Contains(out.String(), "private-deployment-secret") {
		t.Fatalf("deploy ps leaked command or environment data: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "down", "--dry-run", "test-stack"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy down --dry-run: code=%d err=%q", code, errOut.String())
	}
	if first, second := strings.Index(out.String(), "service api"), strings.Index(out.String(), "service database"); first < 0 || second < 0 || first > second {
		t.Fatalf("dry-run did not stop in reverse dependency order: %q", out.String())
	}
	if _, err := state.Get(database.ID); err != nil {
		t.Fatalf("dry-run removed database record: %v", err)
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "down", "--remove", "test-stack"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy down --remove: code=%d err=%q", code, errOut.String())
	}
	for _, record := range []state.Record{database, api} {
		if _, err := state.Get(record.ID); err == nil {
			t.Errorf("deployment record %s survived --remove", record.Name)
		}
	}
	if _, err := state.Get(other.ID); err != nil {
		t.Fatalf("removing test-stack affected another stack: %v", err)
	}
}

func TestDeployPullSkipsCachedImagesAndDeduplicatesCanonicalReferences(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "version"), []byte("deployment image fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	localReference := "registry-1.docker.io/library/alpine:local"
	if _, err := imageStore.ImportRootFS(root, localReference); err != nil {
		t.Fatalf("import test image: %v", err)
	}
	manifest := `{
		"version": 1,
		"name": "prefetch-test",
		"services": {
			"api": {
				"image": "alpine:local",
				"command": ["/bin/true"],
				"options": {}
			},
			"worker": {
				"image": "registry-1.docker.io/library/alpine:local",
				"command": ["/bin/true"],
				"options": {}
			}
		}
	}`
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "pull", path}, &out, &errOut); code != 0 {
		t.Fatalf("deploy pull: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if strings.Count(out.String(), "image alpine:local already present locally") != 1 {
		t.Fatalf("cached image was not reported exactly once: %q", out.String())
	}
	if !strings.Contains(out.String(), "reuses image registry-1.docker.io/library/alpine:local") {
		t.Fatalf("canonical alias was not deduplicated: %q", out.String())
	}
	if !strings.Contains(out.String(), "all 1 image reference(s) are available locally (0 pulled)") {
		t.Fatalf("final prefetch summary missing: %q", out.String())
	}
	if _, err := imageStore.Verify(localReference); err != nil {
		t.Fatalf("cached image was damaged: %v", err)
	}
}

func TestResolveDeploymentImageReferenceFindsCanonicalDockerHubAlias(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("canonical"), 0644); err != nil {
		t.Fatal(err)
	}
	want := "registry-1.docker.io/library/busybox:test"
	if _, err := imageStore.ImportRootFS(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := resolveDeploymentImageReference("busybox:test", "")
	if err != nil {
		t.Fatalf("resolve shorthand image: %v", err)
	}
	if got != want {
		t.Fatalf("resolved reference = %q, want %q", got, want)
	}
}

func TestResolveDeploymentImageReferenceRejectsWrongCachedPlatform(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("foreign-platform"), 0644); err != nil {
		t.Fatal(err)
	}
	foreignArch := "arm64"
	if runtime.GOARCH == "arm64" {
		foreignArch = "amd64"
	}
	if _, err := imageStore.ImportRootFSWithConfig(root, "demo:foreign", "linux", foreignArch, "", imageStore.ImageRuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDeploymentImageReference("demo:foreign", "linux/"+runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "local image platform is") {
		t.Fatalf("expected cached image platform mismatch, got %v", err)
	}
	if got, err := resolveDeploymentImageReference("demo:foreign", "linux/"+foreignArch); err != nil || got != "demo:foreign" {
		t.Fatalf("matching platform should resolve cached image, got %q, error %v", got, err)
	}
}

func TestDeploymentTargetPlatformValidation(t *testing.T) {
	for _, invalid := range []string{"linux", "windows/amd64", "linux/", "linux/amd64/", "linux/amd64/v8/extra", "linux/amd64\n"} {
		if _, _, _, err := deploymentTargetPlatform(invalid); err == nil {
			t.Errorf("invalid platform %q unexpectedly accepted", invalid)
		}
	}
	osName, arch, variant, err := deploymentTargetPlatform("linux/arm64/v8")
	if err != nil || osName != "linux" || arch != "arm64" || variant != "v8" {
		t.Fatalf("unexpected parsed platform: %q %q %q, error %v", osName, arch, variant, err)
	}
}

func TestDeploymentReadinessProbeIsSelectedByProcessService(t *testing.T) {
	databaseProbe := &deploy.ReadinessProbe{TCPAddress: "127.0.0.1:5432", Attempts: 1}
	apiProbe := &deploy.ReadinessProbe{HTTPURL: "http://127.0.0.1:8080/healthz", Attempts: 2}
	file := deploy.File{
		Version: 1,
		Services: map[string]deploy.Service{
			"database": {Command: []string{"/bin/true"}, Readiness: databaseProbe},
			"api":      {Command: []string{"/bin/true"}, Readiness: apiProbe},
		},
	}
	process := &deploymentProcess{service: "api"}
	got, err := deploymentReadinessForProcess(file, process)
	if err != nil {
		t.Fatalf("resolve readiness probe: %v", err)
	}
	if got != apiProbe {
		t.Fatalf("readiness probe = %#v, want API probe %#v", got, apiProbe)
	}
}

func TestDeploymentReadinessRejectsUnknownProcessService(t *testing.T) {
	_, err := deploymentReadinessForProcess(deploy.File{Services: map[string]deploy.Service{}}, &deploymentProcess{service: "missing"})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected unknown service error, got %v", err)
	}
}

func TestDeployWaitRequiresAllServicesAndRedactsRuntimeDetails(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	create := func(service, name string, order int) state.Record {
		t.Helper()
		record, err := state.Create(name, []string{"/bin/sh", "-c", "private-command"}, container.Options{
			Env: []string{"TOKEN=private-wait-secret"},
		}, map[string]string{
			deploy.StackLabel:   "wait-stack",
			deploy.ServiceLabel: service,
			deploy.OrderLabel:   strconv.Itoa(order),
		})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	database := create("database", "wait-stack-database", 0)
	api := create("api", "wait-stack-api", 1)
	if err := state.Start(database.ID, 301); err != nil {
		t.Fatal(err)
	}
	if err := state.Start(api.ID, 302); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "wait", "--condition", "running", "--json", "wait-stack"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy wait running: code=%d stderr=%q", code, errOut.String())
	}
	var result deploymentWaitResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode deploy wait JSON: %v", err)
	}
	if result.Stack != "wait-stack" || result.Condition != "running" || len(result.Services) != 2 {
		t.Fatalf("unexpected deployment wait result: %+v", result)
	}
	if result.Services[0].Service != "database" || result.Services[1].Service != "api" {
		t.Fatalf("deployment wait services are not ordered: %+v", result.Services)
	}
	if strings.Contains(out.String(), "private-command") || strings.Contains(out.String(), "private-wait-secret") {
		t.Fatalf("deployment wait leaked command or environment data: %s", out.String())
	}
	if err := state.Finish(database.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(api.ID, nil); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "wait", "--condition", "stopped", "wait-stack"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy wait stopped: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "reached stopped state") {
		t.Fatalf("stopped confirmation missing: %q", out.String())
	}
}

func TestDeployWaitFailsWhenServiceExitsBeforeRunningAndTimesOut(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("wait-failed", []string{"/bin/false"}, container.Options{}, map[string]string{
		deploy.StackLabel:   "wait-failure-stack",
		deploy.ServiceLabel: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 401); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, &container.ExitError{Code: 1}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "wait", "--condition", "running", "wait-failure-stack"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "reached failed") {
		t.Fatalf("wait running should fail on an exited service: code=%d stderr=%q", code, errOut.String())
	}

	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	_, err = state.Create("not-started", []string{"/bin/true"}, container.Options{}, map[string]string{
		deploy.StackLabel:   "wait-timeout-stack",
		deploy.ServiceLabel: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "wait", "--timeout", "20ms", "--interval", "10ms", "wait-timeout-stack"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "timed out") {
		t.Fatalf("wait running should time out: code=%d stderr=%q", code, errOut.String())
	}
}

func TestDeploymentWaitConditionHandlesEmptyStacksAndInvalidStates(t *testing.T) {
	if done, err := deploymentWaitCondition(nil, "stopped"); err != nil || !done {
		t.Fatalf("empty stopped stack = %v, %v; want true, nil", done, err)
	}
	if _, err := deploymentWaitCondition(nil, "running"); err == nil {
		t.Fatal("empty running stack unexpectedly satisfied")
	}
	if _, err := deploymentWaitCondition(nil, "invalid"); err == nil {
		t.Fatal("invalid wait condition unexpectedly accepted")
	}
}

func TestDeployLogsPrefixesServicesAndFiltersByName(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	create := func(service, name, content string, order int) state.Record {
		t.Helper()
		record, err := state.Create(name, []string{"/bin/service"}, container.Options{}, map[string]string{
			deploy.StackLabel:   "logs-stack",
			deploy.ServiceLabel: service,
			deploy.OrderLabel:   strconv.Itoa(order),
		})
		if err != nil {
			t.Fatal(err)
		}
		logFile, err := state.OpenLog(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := logFile.WriteString(content); err != nil {
			t.Fatal(err)
		}
		if err := logFile.Close(); err != nil {
			t.Fatal(err)
		}
		return record
	}
	create("database", "logs-database", "db ready\ndb accepting\n", 0)
	create("api", "logs-api", "api booting\napi serving\n", 1)
	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "logs", "--tail", "1", "logs-stack"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy logs: code=%d stderr=%q", code, errOut.String())
	}
	want := "[database] db accepting\n[api] api serving\n"
	if out.String() != want {
		t.Fatalf("stack logs = %q, want %q", out.String(), want)
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "logs", "logs-stack", "api"}, &out, &errOut); code != 0 {
		t.Fatalf("deploy logs service filter: code=%d stderr=%q", code, errOut.String())
	}
	if out.String() != "[api] api booting\n[api] api serving\n" {
		t.Fatalf("filtered logs = %q", out.String())
	}
}

func TestDeployLogsFollowDrainsFinalServiceOutput(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("logs-follow-api", []string{"/bin/service"}, container.Options{}, map[string]string{
		deploy.StackLabel:   "logs-follow-stack",
		deploy.ServiceLabel: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 501); err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("initial output\n"); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	type followResult struct {
		code int
		out  string
		err  string
	}
	done := make(chan followResult, 1)
	go func() {
		var out, errOut bytes.Buffer
		code := run([]string{"deploy", "logs", "--follow", "--tail", "0", "--interval", "10ms", "logs-follow-stack"}, &out, &errOut)
		done <- followResult{code: code, out: out.String(), err: errOut.String()}
	}()
	time.Sleep(25 * time.Millisecond)
	logFile, err = state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("final output\npartial"); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.code != 0 {
			t.Fatalf("deploy logs --follow returned %d: %q", result.code, result.err)
		}
		if result.out != "[api] initial output\n[api] final output\n[api] partial" {
			t.Fatalf("followed log output = %q", result.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deploy logs --follow did not finish after the service exited")
	}
}

func TestDeployLogsFollowWaitsForCreatedServiceToStart(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("logs-created-api", []string{"/bin/service"}, container.Options{}, map[string]string{
		deploy.StackLabel:   "logs-created-stack",
		deploy.ServiceLabel: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	type followResult struct {
		code int
		out  string
		err  string
	}
	done := make(chan followResult, 1)
	go func() {
		var out, errOut bytes.Buffer
		code := run([]string{"deploy", "logs", "--follow", "--tail", "0", "--interval", "10ms", "logs-created-stack"}, &out, &errOut)
		done <- followResult{code: code, out: out.String(), err: errOut.String()}
	}()
	time.Sleep(25 * time.Millisecond)
	if err := state.Start(record.ID, 502); err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("service started\n"); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.code != 0 {
			t.Fatalf("deploy logs --follow returned %d: %q", result.code, result.err)
		}
		if result.out != "[api] service started\n" {
			t.Fatalf("followed created-service output = %q", result.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deploy logs --follow stopped watching a created service")
	}
}

func TestDeploymentContainerOptionsResolveNamedVolumes(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	created, err := volume.Create("api-data", map[string]string{"purpose": "test"})
	if err != nil {
		t.Fatal(err)
	}
	service := deploy.Service{
		Command: []string{"/bin/service"},
		Options: container.Options{RootFS: "/srv/boxr/api"},
		Volumes: []deploy.VolumeMount{{Name: "api-data", Target: "/var/lib/api", ReadOnly: true}},
	}
	options, err := deploymentContainerOptions(service)
	if err != nil {
		t.Fatalf("prepare named volume options: %v", err)
	}
	if len(options.BindMounts) != 1 {
		t.Fatalf("bind mounts = %d, want 1", len(options.BindMounts))
	}
	mount := options.BindMounts[0]
	if mount.Source != created.Path || mount.Target != "/var/lib/api" || !mount.ReadOnly {
		t.Fatalf("resolved named volume mount = %+v", mount)
	}
}

func TestDeploymentContainerOptionsRejectMissingNamedVolume(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	_, err := deploymentContainerOptions(deploy.Service{
		Command: []string{"/bin/service"},
		Options: container.Options{RootFS: "/srv/boxr/api"},
		Volumes: []deploy.VolumeMount{{Name: "missing-data", Target: "/var/lib/api"}},
	})
	if err == nil || !strings.Contains(err.Error(), "named volume") {
		t.Fatalf("expected missing volume to fail closed, got %v", err)
	}
}

func TestDeployUpFailsVolumePreflightBeforeCreatingAnyContainer(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	file := deploy.File{
		Version: 1,
		Name:    "volume-preflight",
		Services: map[string]deploy.Service{
			"database": {
				Command: []string{"/bin/true"},
				Options: container.Options{RootFS: "/srv/boxr/database"},
				Volumes: []deploy.VolumeMount{{Name: "missing-db", Target: "/var/lib/db"}},
			},
		},
	}
	plan, err := deploy.BuildPlan(file)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runDeploymentUp(file, plan, time.Second, 0, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "configuration preflight failed") {
		t.Fatalf("deploy up missing volume: code=%d stderr=%q", code, errOut.String())
	}
	records, err := state.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("service records were created before volume preflight completed: %+v", records)
	}
}

func TestVersionJSONOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("version --json: %d %q", code, errOut.String())
	}
	for _, field := range []string{`"version"`, `"commit"`, `"buildDate"`, `"goVersion"`} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("JSON version missing %s: %s", field, out.String())
		}
	}
}

func TestContainerWorkflowCommands(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("web-api", []string{"/bin/true"}, container.Options{Env: []string{"TOKEN=never-persist"}}, map[string]string{"tier": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 201); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"ps", "--all", "--filter", "label=tier=test"}, &out, &errOut); code != 0 {
		t.Fatalf("ps: %d %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "web-api") || !strings.Contains(out.String(), record.ID) {
		t.Fatalf("ps output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"inspect", "web-api"}, &out, &errOut); code != 0 {
		t.Fatalf("inspect: %d %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"status": "exited"`) || strings.Contains(out.String(), "never-persist") {
		t.Fatalf("inspect leaked or missed state: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"events", "--limit", "2", "--format", "json"}, &out, &errOut); code != 0 {
		t.Fatalf("events: %d %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"action": "start"`) || !strings.Contains(out.String(), `"action": "die"`) {
		t.Fatalf("events output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"rm", "web-api"}, &out, &errOut); code != 0 {
		t.Fatalf("rm: %d %q", code, errOut.String())
	}
	if _, err := state.Get(record.ID); err == nil {
		t.Fatal("container record survived rm")
	}
}

func TestPruneCommand(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("old-job", []string{"true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 202); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"prune", "--older-than", "0s"}, &out, &errOut); code != 0 {
		t.Fatalf("prune: %d %q", code, errOut.String())
	}
	if _, err := state.Get(record.ID); err == nil {
		t.Fatal("prune left completed record")
	}
}

func TestRunRejectsInvalidNameAndLabels(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--name", "bad name", "/bin/true"},
		{"run", "--name", "valid", "--label", "missing-value", "/bin/true"},
		{"run", "--name", "valid", "--label", "=empty", "/bin/true"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (%q)", args, code, errOut.String())
		}
	}
}

func TestInfoJSON(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run([]string{"info", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("info: %d %q", code, errOut.String())
	}
	for _, field := range []string{`"goVersion"`, `"architecture"`, `"stateDirectory"`, `"runtimeSupported"`} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("info JSON missing %s: %q", field, out.String())
		}
	}
}

func TestFeaturesCommandJSONInventory(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"features", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("features --json returned %d: %s", code, errOut.String())
	}
	var result struct {
		SchemaVersion int      `json:"schemaVersion"`
		Count         int      `json:"count"`
		Features      []string `json:"features"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode feature inventory: %v", err)
	}
	if result.SchemaVersion != 1 {
		t.Errorf("schema version = %d, want 1", result.SchemaVersion)
	}
	if result.Count != len(result.Features) {
		t.Errorf("reported count %d does not match feature list length %d", result.Count, len(result.Features))
	}
	if result.Count < 100 {
		t.Errorf("feature inventory has %d entries; want at least 100", result.Count)
	}
	matrix, err := os.ReadFile("../../docs/FEATURE_MATRIX.md")
	if err != nil {
		t.Fatalf("read feature matrix: %v", err)
	}
	for index, feature := range result.Features {
		want := fmt.Sprintf("%03d. %s", index+1, feature)
		if !strings.Contains(string(matrix), want) {
			t.Errorf("feature matrix is missing registry entry %q", want)
		}
	}
}

func TestMergeLabelsPrecedenceAndIsolation(t *testing.T) {
	metadata := map[string]string{"owner": "metadata", "tier": "edge"}
	configured := map[string]string{"owner": "config", "team": "runtime"}
	merged := mergeLabels(metadata, configured)
	if merged["owner"] != "config" || merged["tier"] != "edge" || merged["team"] != "runtime" {
		t.Fatalf("unexpected merged labels: %#v", merged)
	}
	merged["new"] = "value"
	if _, exists := metadata["new"]; exists {
		t.Fatal("mergeLabels mutated metadata input")
	}
	if _, exists := configured["new"]; exists {
		t.Fatal("mergeLabels mutated configured labels input")
	}
}

func TestFeaturesRejectsUnexpectedArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"features", "unexpected"}, &out, &errOut); code != 2 {
		t.Fatalf("features unexpected returned %d, want 2", code)
	}
}

func TestLifecycleCommandsLogsWaitAndStop(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("lifecycle", []string{"/bin/sh", "-c", "echo hi"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("first\nsecond\nthird\n"); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	if err := state.Start(record.ID, 0x7fffffff); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, &container.ExitError{Code: 0}); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"logs", "--tail", "2", "lifecycle"}, &out, &errOut); code != 0 || out.String() != "second\nthird\n" {
		t.Fatalf("logs: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"wait", "lifecycle"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != "0" {
		t.Fatalf("wait: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"stop", "lifecycle"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "already exited") {
		t.Fatalf("stop exited: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"kill", "--signal", "BOGUS", "lifecycle"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "not running") {
		t.Fatalf("kill exited: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestLifecycleCommandsRejectInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"stop"}, {"stop", "--time", "-1s", "name"},
		{"kill"}, {"wait"}, {"wait", "--timeout", "-1s", "name"},
		{"logs"}, {"logs", "--tail", "-1", "name"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stderr=%q)", args, code, errOut.String())
		}
	}
}

func TestLogsFollowStreamsAppendedOutputUntilExit(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("follow-log", []string{"worker"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 987654); err != nil {
		t.Fatal(err)
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("initial\n"); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()

	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"logs", "--tail", "0", "--follow", "follow-log"}, &out, &errOut) }()
	time.Sleep(100 * time.Millisecond)
	logFile, err = state.OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("appended\n"); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("logs --follow returned %d: %q", code, errOut.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("logs --follow did not finish after container exit")
	}
	if got, want := out.String(), "initial\nappended\n"; got != want {
		t.Fatalf("follow output = %q, want %q", got, want)
	}
}

func TestStopCommandStopsRecordedProcess(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	child := exec.Command("sh", "-c", "exec sleep 30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start signal test process: %v", err)
	}
	record, err := state.Create("stop-process", []string{"sleep", "30"}, container.Options{}, nil)
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal(err)
	}
	if err := state.Start(record.ID, child.Process.Pid); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() {
		waitErr := child.Wait()
		waitDone <- waitErr
		_ = state.Finish(record.ID, waitErr)
	}()
	var out, errOut bytes.Buffer
	code := run([]string{"stop", "--time", "2s", "stop-process"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("stop returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("child process did not exit after stop")
	}
	finished, err := state.Get(record.ID)
	if err != nil || finished.Status != "failed" || finished.ExitCode == nil || *finished.ExitCode != 143 {
		t.Fatalf("stopped record = %#v, err=%v; expected signal exit 143", finished, err)
	}
}

func TestNamedVolumeCLIWorkflowAndActiveProtection(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BOXR_DATA_DIR", dataDir)
	t.Setenv("BOXR_STATE_DIR", filepath.Join(dataDir, "state"))
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "create", "--label", "app=cache", "cache"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != "cache" {
		t.Fatalf("volume create: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	item, err := volume.Get("cache")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Path, "keep.dat"), []byte("persistent"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "ls", "--quiet"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != "cache" {
		t.Fatalf("volume ls: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "inspect", "cache"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), item.Path) {
		t.Fatalf("volume inspect: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	record, err := state.Create("using-volume", []string{"sleep", "30"}, container.Options{BindMounts: []container.BindMount{{Source: item.Path, Target: "/cache"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 123456); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "rm", "cache"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "active container") {
		t.Fatalf("active volume removal: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(filepath.Join(item.Path, "keep.dat")); err != nil {
		t.Fatalf("active volume data was removed: %v", err)
	}
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "rm", "cache"}, &out, &errOut); code != 0 {
		t.Fatalf("volume rm after exit: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(item.Path); !os.IsNotExist(err) {
		t.Fatalf("volume data still exists after removal: %v", err)
	}
}

func TestNamedVolumeParsingAndRunRequiresExistingVolume(t *testing.T) {
	name, mount, err := parseVolumeMount("cache:/var/cache:ro")
	if err != nil || name != "cache" || mount.Target != "/var/cache" || !mount.ReadOnly {
		t.Fatalf("parseVolumeMount() = %q %#v %v", name, mount, err)
	}
	for _, value := range []string{"", "cache:relative", "cache:/", "cache:/data:bad", "../cache:/data", "cache:/data:ro:extra"} {
		if _, _, err := parseVolumeMount(value); err == nil {
			t.Errorf("parseVolumeMount(%q) unexpectedly succeeded", value)
		}
	}
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--volume", "missing:/cache", "/bin/true"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "volume") {
		t.Fatalf("run with missing volume: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestVolumePruneRequiresExplicitConfirmation(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	if _, err := volume.Create("unused", nil); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "prune"}, &out, &errOut); code != 2 {
		t.Fatalf("volume prune without force returned %d", code)
	}
	if code := run([]string{"volume", "prune", "--force"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "removed 1") {
		t.Fatalf("volume prune force: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestVolumeFiltersAndExport(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BOXR_DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("BOXR_STATE_DIR", filepath.Join(base, "state"))
	first, err := volume.Create("frontend", map[string]string{"tier": "web", "region": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "index.html"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := volume.Create("backend", map[string]string{"tier": "api"}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "ls", "--quiet", "--filter", "label=tier=web"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != "frontend" {
		t.Fatalf("volume label filter: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "ls", "--quiet", "--filter", "name=backend"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != "backend" {
		t.Fatalf("volume name filter: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	archive := filepath.Join(base, "frontend.tar.gz")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "export", "--json", "frontend", archive}, &out, &errOut); code != 0 {
		t.Fatalf("volume export: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	var exported volume.Archive
	if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
		t.Fatalf("decode volume export metadata: %v", err)
	}
	if exported.Volume != "frontend" || !strings.HasPrefix(exported.Digest, "sha256:") || exported.SizeBytes <= 0 {
		t.Fatalf("volume export metadata is incomplete: %+v", exported)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "export", "frontend", archive}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "already exists") {
		t.Fatalf("volume export overwrote existing archive: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	extracted := filepath.Join(base, "restored")
	if _, err := rootfs.ExtractTarGz(archive, extracted, rootfs.Limits{}); err != nil {
		t.Fatalf("extract volume export: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(extracted, "index.html"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("exported content = %q, %v", data, err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "prune", "--force", "--filter", "label=tier=web"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "removed 1") {
		t.Fatalf("filtered volume prune: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if _, err := volume.Get("frontend"); err == nil {
		t.Fatal("filtered volume prune kept the matching volume")
	}
	if _, err := volume.Get("backend"); err != nil {
		t.Fatalf("filtered volume prune removed a non-matching volume: %v", err)
	}
}

func TestVolumeCloneCopiesDataAndLabels(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	source, err := volume.Create("original", map[string]string{"origin": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Path, "state.db"), []byte("snapshot-data"), 0640); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "clone", "--label", "copy=one", "original", "copy"}, &out, &errOut); code != 0 {
		t.Fatalf("clone: %s", errOut.String())
	}
	cloned, err := volume.Get("copy")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cloned.Path, "state.db"))
	if err != nil || string(data) != "snapshot-data" {
		t.Fatalf("cloned file = %q, %v", data, err)
	}
	if cloned.Labels["copy"] != "one" {
		t.Fatalf("clone labels = %#v", cloned.Labels)
	}
}

func TestVolumeCloneRefusesActiveSource(t *testing.T) {
	data := t.TempDir()
	t.Setenv("BOXR_DATA_DIR", data)
	t.Setenv("BOXR_STATE_DIR", filepath.Join(data, "state"))
	source, err := volume.Create("busy", nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := state.Create("busy-run", []string{"/bin/sh"}, container.Options{BindMounts: []container.BindMount{{Source: source.Path, Target: "/data"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 424242); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "clone", "busy", "copy"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "mounted by active container") {
		t.Fatalf("clone active source: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if _, err := volume.Get("copy"); err == nil {
		t.Fatal("active source clone unexpectedly created target")
	}
}

func TestVolumeUsageDoesNotFollowSymlinks(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	item, err := volume.Create("usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(item.Path, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Path, "nested", "file"), []byte("12345"), 0640); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "outside"), []byte("do not count"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(item.Path, "external")); err != nil {
		t.Fatal(err)
	}
	usage, err := volume.Measure("usage")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Files != 1 || usage.Directories != 1 || usage.Symlinks != 1 || usage.FileBytes != 5 || usage.SymlinkBytes != int64(len(target)) || usage.TotalBytes != 5+int64(len(target)) || !usage.Snapshot {
		t.Fatalf("unexpected volume usage: %+v", usage)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "du", "--json", "usage"}, &out, &errOut); code != 0 {
		t.Fatalf("volume du: %s", errOut.String())
	}
	if !strings.Contains(out.String(), `"snapshot": true`) {
		t.Fatalf("JSON usage omitted snapshot warning: %s", out.String())
	}
}

func TestVolumeImportRestoresPortableExport(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	created, err := volume.Create("source", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "payload.txt"), []byte("portable volume"), 0640); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "volume.tar.gz")
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "export", "source", archive}, &out, &errOut); code != 0 {
		t.Fatalf("export: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"volume", "import", "--label", "restored=yes", archive, "restored"}, &out, &errOut); code != 0 {
		t.Fatalf("import: %s", errOut.String())
	}
	imported, err := volume.Get("restored")
	if err != nil {
		t.Fatal(err)
	}
	if imported.Labels["restored"] != "yes" {
		t.Fatalf("import metadata labels = %#v", imported.Labels)
	}
	data, err := os.ReadFile(filepath.Join(imported.Path, "payload.txt"))
	if err != nil || string(data) != "portable volume" {
		t.Fatalf("restored payload = %q, %v", data, err)
	}
}

func TestVolumeImportRejectsInvalidArchiveWithoutLeavingVolume(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	invalid := filepath.Join(t.TempDir(), "invalid.tar.gz")
	if err := os.WriteFile(invalid, []byte("not gzip"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "import", invalid, "bad"}, &out, &errOut); code != 1 {
		t.Fatalf("volume import returned %d, want 1: %s", code, errOut.String())
	}
	if _, err := volume.Get("bad"); err == nil {
		t.Fatal("failed import left an accessible volume")
	}
}

func TestVolumeListRejectsUnknownFilter(t *testing.T) {
	t.Setenv("BOXR_DATA_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run([]string{"volume", "ls", "--filter", "unknown=value"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unsupported filter") {
		t.Fatalf("unknown volume filter: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestEventsFiltersBeforeApplyingLimit(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	first, err := state.Create("web-old", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(first.ID, nil); err != nil {
		t.Fatal(err)
	}
	second, err := state.Create("web-new", []string{"/bin/false"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(second.ID, fmt.Errorf("failed")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"events", "--action", "die", "--container", "web-new", "--limit", "1", "--format", "json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("events returned %d: %s", code, errOut.String())
	}
	var events []state.Event
	if err := json.Unmarshal(out.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != second.ID || events[0].Action != "die" {
		t.Fatalf("unexpected filtered event list: %+v", events)
	}
}

func TestParseEventsSince(t *testing.T) {
	before := time.Now().Add(-time.Hour)
	parsed, err := parseEventsSince("15m")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Before(before) || parsed.After(time.Now()) {
		t.Fatalf("unexpected duration threshold: %s", parsed)
	}
	stamp := time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)
	parsed, err = parseEventsSince(stamp.Format(time.RFC3339))
	if err != nil || !parsed.Equal(stamp) {
		t.Fatalf("RFC3339 parse = %s, %v", parsed, err)
	}
	for _, invalid := range []string{"-1h", "tomorrow", "1x"} {
		if _, err := parseEventsSince(invalid); err == nil {
			t.Errorf("invalid since %q unexpectedly accepted", invalid)
		}
	}
}

func TestImageCommandLifecycle(t *testing.T) {
	store := filepath.Join(t.TempDir(), "image-store")
	t.Setenv("BOXR_IMAGE_DIR", store)
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "hello"), []byte("hello from image\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "import", root, "demo:test"}, &out, &errOut); code != 0 {
		t.Fatalf("image import code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "ls", "--format", "json"}, &out, &errOut); code != 0 {
		t.Fatalf("image ls code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "demo:test") {
		t.Fatalf("image list missing tag: %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "inspect", "demo:test"}, &out, &errOut); code != 0 {
		t.Fatalf("image inspect code=%d stderr=%s", code, errOut.String())
	}
	archive := filepath.Join(t.TempDir(), "image.oci.tar.gz")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "save", "demo:test", archive}, &out, &errOut); code != 0 {
		t.Fatalf("image save code=%d stderr=%s", code, errOut.String())
	}
	if code := run([]string{"image", "tag", "demo:test", "demo:latest"}, &out, &errOut); code != 0 {
		t.Fatalf("image tag code=%d stderr=%s", code, errOut.String())
	}
	dockerArchive := filepath.Join(t.TempDir(), "image.docker.tar")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "save", "--format", "docker", "demo:test", "demo:latest", dockerArchive}, &out, &errOut); code != 0 {
		t.Fatalf("Docker image save code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "rm", "demo:test", "demo:latest"}, &out, &errOut); code != 0 {
		t.Fatalf("image rm code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "load", dockerArchive}, &out, &errOut); code != 0 {
		t.Fatalf("Docker image load code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "inspect", "demo:latest"}, &out, &errOut); code != 0 {
		t.Fatalf("multi-image Docker load omitted second tag: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "rm", "demo:test", "demo:latest"}, &out, &errOut); code != 0 {
		t.Fatalf("image rm after Docker load code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "load", archive}, &out, &errOut); code != 0 {
		t.Fatalf("image load code=%d stderr=%s", code, errOut.String())
	}
	destination := filepath.Join(t.TempDir(), "unpacked")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "unpack", "demo:test", destination}, &out, &errOut); code != 0 {
		t.Fatalf("image unpack code=%d stderr=%s", code, errOut.String())
	}
	contents, err := os.ReadFile(filepath.Join(destination, "bin", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "hello from image\n" {
		t.Fatalf("unexpected unpacked content %q", contents)
	}
}

func TestPSQuietLastAndNoTrunc(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	older, err := state.Create("older", []string{"/bin/echo", strings.Repeat("x", 80)}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	newer, err := state.Create("newer", []string{"/bin/echo", strings.Repeat("y", 80)}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"ps", "--last", "1", "--quiet"}, &out, &errOut); code != 0 {
		t.Fatalf("ps quiet: %s", errOut.String())
	}
	if strings.TrimSpace(out.String()) != newer.ID || strings.Contains(out.String(), older.ID) {
		t.Fatalf("unexpected quiet last output: %q", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"ps", "--all", "--no-trunc"}, &out, &errOut); code != 0 {
		t.Fatalf("ps no-trunc: %s", errOut.String())
	}
	if !strings.Contains(out.String(), strings.Repeat("x", 80)) || !strings.Contains(out.String(), strings.Repeat("y", 80)) {
		t.Fatalf("command was truncated: %s", out.String())
	}
}

func TestImageListFiltersAreCombined(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	for _, spec := range []struct{ name, body string }{{"alpha", "a"}, {"beta", "b"}} {
		root := filepath.Join(t.TempDir(), spec.name)
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "item"), []byte(spec.body), 0644); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if code := run([]string{"image", "import", root, "demo:" + spec.name}, &out, &errOut); code != 0 {
			t.Fatalf("import %s: %s", spec.name, errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "ls", "--quiet", "--filter", "reference=demo:a"}, &out, &errOut); code != 0 {
		t.Fatalf("image ls filter: %s", errOut.String())
	}
	if strings.TrimSpace(out.String()) != "demo:alpha" {
		t.Fatalf("unexpected filtered image list: %q", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "ls", "--filter", "unknown=x"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown image filter returned %d: %s", code, errOut.String())
	}
}

func TestImageVerifyAndUsageCommands(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("content-addressed"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "import", root, "demo:verify"}, &out, &errOut); code != 0 {
		t.Fatalf("import: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "verify", "demo:verify"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "verified demo:verify") {
		t.Fatalf("verify: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "du", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("image du: %s", errOut.String())
	}
	var usage struct {
		Images           int   `json:"images"`
		UniqueBlobs      int   `json:"uniqueBlobs"`
		ReachableBytes   int64 `json:"reachableBytes"`
		ReclaimableBytes int64 `json:"reclaimableBytes"`
	}
	if err := json.Unmarshal(out.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Images != 1 || usage.UniqueBlobs != 3 || usage.ReachableBytes <= 0 || usage.ReclaimableBytes != 0 {
		t.Fatalf("unexpected image usage: %+v", usage)
	}
}

func TestImagePruneRequiresExplicitMode(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "prune"}, &out, &errOut); code != 2 {
		t.Fatalf("image prune default code=%d want 2", code)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "prune", "--dry-run"}, &out, &errOut); code != 0 {
		t.Fatalf("image prune dry-run code=%d stderr=%s", code, errOut.String())
	}
}

func TestRunDryRunDoesNotStartOrPersistSecrets(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BOXR_DATA_DIR", dataDir)
	t.Setenv("BOXR_STATE_DIR", filepath.Join(dataDir, "state"))
	const secretEnv = "super-secret-env-token"
	const secretArg = "sensitive-command-argument"
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--dry-run", "--name", "preview", "--env", "API_TOKEN=" + secretEnv, "--label", "owner=platform", "/bin/echo", secretArg}, &out, &errOut)
	if code != 0 {
		t.Fatalf("dry run returned %d: %s", code, errOut.String())
	}
	for _, forbidden := range []string{secretEnv, secretArg, "platform"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("dry-run output leaked %q: %s", forbidden, out.String())
		}
	}
	for _, required := range []string{`"mode": "dry-run"`, `"command": "/bin/echo"`, `"argumentCount": 1`, `"API_TOKEN"`, `"redacted"`, `"noContainerStateCreated": true`} {
		if !strings.Contains(out.String(), required) {
			t.Errorf("dry-run output missing %q: %s", required, out.String())
		}
	}
	records, err := state.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("dry run created %d state record(s)", len(records))
	}
}

func TestRunImageDryRunVerifiesAndReportsLocalImage(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello"), []byte("container image"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "import", root, "demo:run"}, &out, &errOut); code != 0 {
		t.Fatalf("image import: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"run", "--image", "demo:run", "--dry-run", "--", "/bin/sh"}, &out, &errOut); code != 0 {
		t.Fatalf("image dry-run: %s", errOut.String())
	}
	var plan struct {
		ImageReference   string   `json:"imageReference"`
		RootFSConfigured bool     `json:"rootfsConfigured"`
		NoStateCreated   bool     `json:"noContainerStateCreated"`
		LabelKeys        []string `json:"labelKeys"`
	}
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("decode dry-run plan: %v (%s)", err, out.String())
	}
	if plan.ImageReference != "demo:run" || !plan.RootFSConfigured || !plan.NoStateCreated {
		t.Fatalf("unexpected image run plan: %+v", plan)
	}
	if !strings.Contains(strings.Join(plan.LabelKeys, ","), "io.boxr.image") {
		t.Fatalf("image provenance label missing: %+v", plan.LabelKeys)
	}
	if strings.Contains(out.String(), "container image") {
		t.Fatal("dry-run unexpectedly exposed image contents")
	}
	records, err := state.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("image dry-run created %d state records", len(records))
	}
}

func TestRunImageRejectsMissingImageAndRootFSConflict(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--image", "missing:latest", "--dry-run", "/bin/true"}, &out, &errOut); code != 1 {
		t.Fatalf("missing image returned %d, want 1: %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"run", "--image", "demo:latest", "--rootfs", "/tmp/rootfs", "--dry-run", "/bin/true"}, &out, &errOut); code != 2 {
		t.Fatalf("image/rootfs conflict returned %d, want 2: %s", code, errOut.String())
	}
}

func TestRunDryRunStillValidatesOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--dry-run", "--memory", "0", "/bin/true"}, &out, &errOut); code != 2 {
		t.Fatalf("invalid dry-run options returned %d, want 2: %s", code, errOut.String())
	}
}

func TestRunRejectsUnknownSeccompProfileBeforeStarting(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--seccomp-profile", "unrestricted", "/bin/true"}, &out, &errOut); code != 2 {
		t.Fatalf("run with unknown seccomp profile returned %d, want 2; stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "seccomp profile") {
		t.Fatalf("unexpected error: %s", errOut.String())
	}
}

func TestImageTagCommandSharesImage(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello"), []byte("tagged"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "import", root, "demo:source"}, &out, &errOut); code != 0 {
		t.Fatalf("import: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "tag", "demo:source", "demo:alias"}, &out, &errOut); code != 0 {
		t.Fatalf("tag: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "inspect", "demo:alias"}, &out, &errOut); code != 0 {
		t.Fatalf("inspect alias: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "demo:alias") {
		t.Fatalf("alias not shown: %s", out.String())
	}
}

func TestRootFSAuditJSONAndStrictMode(t *testing.T) {
	root := t.TempDir()
	unsafeFile := filepath.Join(root, "shared.txt")
	if err := os.WriteFile(unsafeFile, []byte("shared"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeFile, 0666); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"rootfs", "audit", "--json", root}, &out, &errOut); code != 0 {
		t.Fatalf("rootfs audit JSON returned %d: %s", code, errOut.String())
	}
	var report struct {
		EntriesScanned int64 `json:"entriesScanned"`
		WarningCount   int   `json:"warningCount"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode rootfs audit report: %v", err)
	}
	if report.EntriesScanned != 1 || report.WarningCount != 1 {
		t.Fatalf("unexpected audit report: %+v", report)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"rootfs", "audit", "--strict", root}, &out, &errOut); code != 1 {
		t.Fatalf("strict audit returned %d, want 1: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestRootFSAuditCleanTreePassesStrictMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clean.txt"), []byte("clean"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"rootfs", "audit", "--strict", root}, &out, &errOut); code != 0 {
		t.Fatalf("strict audit rejected clean tree: %s", errOut.String())
	}
}

func TestImageAuditChecksImportedRootfs(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	cleanPath := filepath.Join(root, "image.txt")
	if err := os.WriteFile(cleanPath, []byte("clean image"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "import", root, "demo:audit"}, &out, &errOut); code != 0 {
		t.Fatalf("image import: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "audit", "--json", "demo:audit"}, &out, &errOut); code != 0 {
		t.Fatalf("image audit: %s", errOut.String())
	}
	var report struct {
		Image struct {
			Reference string `json:"reference"`
			OS        string `json:"os"`
			Arch      string `json:"architecture"`
		} `json:"image"`
		IntegrityVerified bool `json:"integrityVerified"`
		LayerCount        int  `json:"layerCount"`
		RootFS            struct {
			Root         string `json:"root"`
			WarningCount int    `json:"warningCount"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode image audit: %v (%s)", err, out.String())
	}
	if report.Image.Reference != "demo:audit" || report.RootFS.Root != "demo:audit" || report.RootFS.WarningCount != 0 || !report.IntegrityVerified || report.LayerCount != 1 || report.Image.OS != "linux" {
		t.Fatalf("unexpected image audit report: %+v", report)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "audit", "--strict", "demo:audit"}, &out, &errOut); code != 0 {
		t.Fatalf("strict image audit rejected clean image: %s", errOut.String())
	}
}

func TestImageBuildCommandCreatesVerifiedImage(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	contextDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(contextDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "bin", "hello"), []byte("built from CLI\n"), 0755); err != nil {
		t.Fatal(err)
	}
	recipe := `{"version":1,"base":"scratch","copy":[{"source":"bin/hello","destination":"/usr/bin/hello"}],"config":{"WorkingDir":"/srv/app","Env":["MODE=test"]}}`
	recipePath := filepath.Join(contextDir, "Boxrfile.json")
	if err := os.WriteFile(recipePath, []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "build", "--file", recipePath, "--platform", "linux/arm64/v8", "--tag", "demo:cli", "--json", contextDir}, &out, &errOut); code != 0 {
		t.Fatalf("image build returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var result struct {
		Reference string `json:"reference"`
		Digest    string `json:"digest"`
		OS        string `json:"os"`
		Arch      string `json:"architecture"`
		Variant   string `json:"variant"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode image build output: %v (%s)", err, out.String())
	}
	if result.Reference != "demo:cli" || !strings.HasPrefix(result.Digest, "sha256:") || result.OS != "linux" || result.Arch != "arm64" || result.Variant != "v8" {
		t.Fatalf("unexpected image build output: %+v", result)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "verify", "demo:cli"}, &out, &errOut); code != 0 {
		t.Fatalf("image verify after build returned %d: %s", code, errOut.String())
	}
}

func TestImageBuildRequiresTagAndRejectsUnknownRecipeFields(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "build"}, &out, &errOut); code != 2 {
		t.Fatalf("image build without tag returned %d, want 2: %s", code, errOut.String())
	}
	contextDir := t.TempDir()
	recipePath := filepath.Join(contextDir, "Boxrfile.json")
	if err := os.WriteFile(recipePath, []byte(`{"version":1,"base":"scratch","run":"echo unsafe"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	if code := run([]string{"image", "build", "--file", recipePath, "--tag", "demo:unsafe", contextDir}, &out, &errOut); code != 1 {
		t.Fatalf("image build with unknown RUN field returned %d, want 1: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "unknown field") {
		t.Fatalf("unexpected unsafe build error: %s", errOut.String())
	}
	validRecipe := `{"version":1,"base":"scratch"}`
	if err := os.WriteFile(recipePath, []byte(validRecipe), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "build", "--platform", "windows/amd64", "--file", recipePath, "--tag", "demo:wrong-platform", contextDir}, &out, &errOut); code != 1 {
		t.Fatalf("image build with non-Linux target returned %d, want 1: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "platform must be linux/ARCH") {
		t.Fatalf("unexpected target-platform error: %s", errOut.String())
	}
}

func TestRunImageAppliesRuntimeDefaultsAndOverrides(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "entrypoint"), []byte("not executed during dry-run"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := imageStore.ImportRootFSWithConfig(root, "demo:defaults", "linux", runtime.GOARCH, "", imageStore.ImageRuntimeConfig{
		Env:        []string{"MODE=image", "KEEP=from-image"},
		Entrypoint: []string{"/entrypoint"},
		Cmd:        []string{"--serve"},
		WorkingDir: "/srv/app",
		Labels:     map[string]string{"tier": "image", "component": "api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--image", "demo:defaults", "--env", "MODE=cli", "--label", "tier=cli", "--dry-run"}, &out, &errOut); code != 0 {
		t.Fatalf("image runtime dry-run returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("decode image runtime plan: %v (%s)", err, out.String())
	}
	if plan.Command != "/entrypoint" || plan.ArgumentCount != 1 || plan.WorkingDirectory != "/srv/app" {
		t.Fatalf("image defaults not applied to command/workdir: %+v", plan)
	}
	if !reflect.DeepEqual(plan.EnvironmentKeys, []string{"KEEP", "MODE"}) {
		t.Fatalf("image environment defaults/overrides missing: %+v", plan.EnvironmentKeys)
	}
	for _, key := range []string{"component", "io.boxr.image", "tier"} {
		if !containsString(plan.LabelKeys, key) {
			t.Errorf("image/runtime label %q missing: %+v", key, plan.LabelKeys)
		}
	}
	if strings.Contains(out.String(), "from-image") || strings.Contains(out.String(), "MODE=image") || strings.Contains(out.String(), "MODE=cli") {
		t.Fatal("dry-run exposed image or environment values")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestImageDiffCommandReportsChangesAsJSON(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	before := t.TempDir()
	if err := os.WriteFile(filepath.Join(before, "version"), []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageStore.ImportRootFS(before, "demo:one"); err != nil {
		t.Fatal(err)
	}
	after := t.TempDir()
	if err := os.WriteFile(filepath.Join(after, "version"), []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after, "new-file"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageStore.ImportRootFS(after, "demo:two"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "diff", "--json", "demo:one", "demo:two"}, &out, &errOut); code != 0 {
		t.Fatalf("image diff returned %d: %s", code, errOut.String())
	}
	var changes []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out.Bytes(), &changes); err != nil {
		t.Fatalf("decode image diff JSON: %v (%s)", err, out.String())
	}
	got := make(map[string]string, len(changes))
	for _, change := range changes {
		got[change.Path] = change.Status
	}
	if got["version"] != "modified" || got["new-file"] != "added" {
		t.Fatalf("unexpected image diff: %#v", changes)
	}
}

func TestImageLockAndVerifyLockCommands(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "release"), []byte("immutable build"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageStore.ImportRootFSWithConfig(root, "demo:pinned", "linux", "arm64", "v8", imageStore.ImageRuntimeConfig{Labels: map[string]string{"org.example.build": "42"}}); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "boxr.lock.json")
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "lock", "--json", "demo:pinned", lockPath}, &out, &errOut); code != 0 {
		t.Fatalf("image lock returned %d: %s", code, errOut.String())
	}
	var lock struct {
		Reference    string `json:"reference"`
		Digest       string `json:"digest"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	if err := json.Unmarshal(out.Bytes(), &lock); err != nil {
		t.Fatalf("decode image lock: %v (%s)", err, out.String())
	}
	if lock.Reference != "demo:pinned" || !strings.HasPrefix(lock.Digest, "sha256:") || lock.Architecture != "arm64" || lock.Variant != "v8" {
		t.Fatalf("unexpected image lock: %+v", lock)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "verify-lock", "--json", lockPath}, &out, &errOut); code != 0 {
		t.Fatalf("image verify-lock returned %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), lock.Digest) {
		t.Fatalf("verified lock output does not include pinned digest: %s", out.String())
	}
}

func TestImageSBOMCommandReportsFileHashesAndPlatform(t *testing.T) {
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(t.TempDir(), "images"))
	root := t.TempDir()
	content := []byte("SBOM CLI test\n")
	if err := os.WriteFile(filepath.Join(root, "hello"), content, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageStore.ImportRootFSWithConfig(root, "demo:sbom-cli", "linux", "arm64", "v8", imageStore.ImageRuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"image", "sbom", "--max-entries", "20", "demo:sbom-cli"}, &out, &errOut); code != 0 {
		t.Fatalf("image sbom returned %d: %s", code, errOut.String())
	}
	var report struct {
		Kind         string `json:"kind"`
		Reference    string `json:"reference"`
		Architecture string `json:"architecture"`
		Entries      []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode SBOM: %v (%s)", err, out.String())
	}
	if report.Kind != "boxr.filesystem-inventory" || report.Reference != "demo:sbom-cli" || report.Architecture != "arm64" || len(report.Entries) != 1 || report.Entries[0].Path != "hello" || report.Entries[0].SHA256 == "" {
		t.Fatalf("unexpected SBOM result: %+v", report)
	}
}

func TestRunRestrictedSecurityProfileCannotBeRelaxedByFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{
		"run", "--dry-run", "--security-profile", "restricted",
		"--network", "host", "--userns=false", "--no-new-privileges=false",
		"--seccomp-profile=", "--cap-drop", "CHOWN", "/bin/true",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("restricted dry-run returned %d: %s", code, errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("decode dry-run plan: %v", err)
	}
	if plan.SecurityProfile != "restricted" || plan.NetworkMode != "none" || !plan.UserNamespace || !plan.NoNewPrivileges || plan.SeccompProfile != "baseline" || plan.DropCapabilityCount < 10 {
		t.Fatalf("restricted profile was not enforced: %#v", plan)
	}
}

func TestRunUntrustedSecurityProfileRequiresRootFSAndLocksItDown(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--dry-run", "--security-profile", "untrusted", "/bin/true"}, &out, &errOut); code != 2 {
		t.Fatalf("untrusted profile without rootfs returned %d: %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"run", "--dry-run", "--security-profile", "untrusted", "--rootfs", "/tmp/boxr-test-rootfs", "--network", "host", "--read-only-rootfs=false", "/bin/true"}, &out, &errOut); code != 0 {
		t.Fatalf("untrusted profile dry-run returned %d: %s", code, errOut.String())
	}
	var plan struct {
		SecurityProfile     string `json:"securityProfile"`
		NetworkMode         string `json:"networkMode"`
		RootFSConfigured    bool   `json:"rootfsConfigured"`
		UserNamespace       bool   `json:"userNamespace"`
		NoNewPrivileges     bool   `json:"noNewPrivileges"`
		SeccompProfile      string `json:"seccompProfile"`
		DropCapabilityCount int    `json:"dropCapabilityCount"`
	}
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SecurityProfile != "untrusted" || plan.NetworkMode != "none" || !plan.RootFSConfigured || !plan.UserNamespace || !plan.NoNewPrivileges || plan.SeccompProfile != "baseline" || plan.DropCapabilityCount != 1 {
		t.Fatalf("untrusted profile was not enforced: %#v", plan)
	}
}

func TestConfigSchemaCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"config", "schema"}, &out, &errOut); code != 0 {
		t.Fatalf("config schema returned %d: %s", code, errOut.String())
	}
	var schema map[string]any
	if err := json.Unmarshal(out.Bytes(), &schema); err != nil {
		t.Fatalf("config schema is invalid JSON: %v", err)
	}
	if schema["$schema"] == nil || schema["properties"] == nil {
		t.Fatalf("config schema is incomplete: %#v", schema)
	}
}

func TestSystemDiskUsageJSONIncludesImageVolumeAndContainerState(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BOXR_IMAGE_DIR", filepath.Join(base, "images"))
	t.Setenv("BOXR_DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("BOXR_STATE_DIR", filepath.Join(base, "state"))
	root := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "release"), []byte("boxr\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageStore.ImportRootFS(root, "df-demo:latest"); err != nil {
		t.Fatal(err)
	}
	vol, err := volume.Create("df-volume", map[string]string{"purpose": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vol.Path, "data.bin"), []byte("volume-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Create("df-container", []string{"/bin/true"}, container.Options{}, nil); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"system", "df", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("system df returned %d: %s", code, errOut.String())
	}
	var report struct {
		Images struct {
			Images     int   `json:"images"`
			TotalBytes int64 `json:"totalBytes"`
		} `json:"images"`
		Volumes        []volume.Usage `json:"volumes"`
		ContainerState directoryUsage `json:"containerState"`
		VolumeBytes    int64          `json:"volumeBytes"`
		TotalBytes     int64          `json:"totalBytes"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode df report: %v", err)
	}
	if report.Images.Images != 1 || report.Images.TotalBytes <= 0 || len(report.Volumes) != 1 || report.VolumeBytes != report.Volumes[0].TotalBytes || report.ContainerState.Files == 0 || report.TotalBytes <= report.Images.TotalBytes {
		t.Fatalf("unexpected system df report: %#v", report)
	}
}

func TestMeasureDirectoryDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("not counted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	usage, err := measureDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Symlinks != 1 || usage.Files != 0 || usage.Bytes != int64(len(outside)) {
		t.Fatalf("symlink was followed or not measured safely: %#v", usage)
	}
	if _, err := measureDirectory(filepath.Join(root, "missing")); err != nil {
		t.Fatalf("missing directory should be empty: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "root-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := measureDirectory(filepath.Join(root, "root-link")); err == nil {
		t.Fatal("symlinked root was accepted")
	}
}

func TestRunResourceProfileFillsUnsetLimitsButKeepsExplicitFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--dry-run", "--resource-profile", "micro", "--memory", "128m", "--cpus", "1.5", "/bin/true"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("resource profile dry-run returned %d: %s", code, errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.ResourceProfile != "micro" || plan.MemoryLimitBytes != 128<<20 || plan.CPUs != 1.5 || plan.PIDsLimit != 64 || plan.CPUWeight != 50 || plan.IOWeight != 50 {
		t.Fatalf("resource profile did not preserve explicit values and fill defaults: %#v", plan)
	}
}

func TestImageListFiltersVariantDigestAndLabels(t *testing.T) {
	items := []imageStore.Image{
		{Reference: "team/api:latest", Digest: "sha256:abcdef012345", OS: "linux", Arch: "arm64", Variant: "v8", RuntimeConfig: imageStore.ImageRuntimeConfig{Labels: map[string]string{"team": "platform", "tier": "prod"}}},
		{Reference: "team/api:dev", Digest: "sha256:123456abcdef", OS: "linux", Arch: "amd64", RuntimeConfig: imageStore.ImageRuntimeConfig{Labels: map[string]string{"team": "platform", "tier": "dev"}}},
	}
	for _, test := range []struct {
		filter string
		want   int
	}{
		{filter: "variant=v8", want: 1},
		{filter: "digest=sha256:abcdef", want: 1},
		{filter: "label=team", want: 2},
		{filter: "label=tier=prod", want: 1},
		{filter: "arch=arm64", want: 1},
	} {
		got, err := filterImageList(items, []string{test.filter})
		if err != nil {
			t.Errorf("filter %q: %v", test.filter, err)
			continue
		}
		if len(got) != test.want {
			t.Errorf("filter %q returned %d images, want %d", test.filter, len(got), test.want)
		}
	}
	if _, err := filterImageList(items, []string{"unknown=value"}); err == nil {
		t.Fatal("unknown filter unexpectedly accepted")
	}
	if _, err := filterImageList(items, []string{"label==bad"}); err == nil {
		t.Fatal("empty label key unexpectedly accepted")
	}
}

func TestDeployProfileFlagsFilterPlanAndDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	manifest := `{
		"version": 1,
		"name": "profile-stack",
		"services": {
			"database": {"command": ["/bin/database"]},
			"api": {
				"command": ["/bin/api"],
				"dependsOn": ["database"],
				"profiles": ["frontend"]
			},
			"metrics": {
				"command": ["/bin/metrics"],
				"profiles": ["observability"]
			}
		}
	}`
	if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "plan", "--json", path}, &out, &errOut); code != 0 {
		t.Fatalf("default deploy plan: code=%d stderr=%q", code, errOut.String())
	}
	var defaultPlan deploy.Plan
	if err := json.Unmarshal(out.Bytes(), &defaultPlan); err != nil {
		t.Fatalf("decode default deployment plan: %v", err)
	}
	if len(defaultPlan.Services) != 1 || defaultPlan.Services[0].Name != "database" {
		t.Fatalf("default plan services = %+v, want only database", defaultPlan.Services)
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "plan", "--profile", "observability", "--profile", "frontend", "--json", path}, &out, &errOut); code != 0 {
		t.Fatalf("profile deploy plan: code=%d stderr=%q", code, errOut.String())
	}
	var selectedPlan deploy.Plan
	if err := json.Unmarshal(out.Bytes(), &selectedPlan); err != nil {
		t.Fatalf("decode profile deployment plan: %v", err)
	}
	if len(selectedPlan.Services) != 3 || !reflect.DeepEqual(selectedPlan.Profiles, []string{"frontend", "observability"}) {
		t.Fatalf("profile plan has unexpected selection: %+v", selectedPlan)
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "up", "--profile", "frontend", "--dry-run", path}, &out, &errOut); code != 0 {
		t.Fatalf("profile deploy up dry-run: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "2 services") || !strings.Contains(out.String(), "Dry run:") {
		t.Fatalf("profile dry-run did not show the filtered service count: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "plan", "--profile", "missing", path}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "unknown deployment profile") {
		t.Fatalf("unknown profile result: code=%d stderr=%q", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "pull", "--profile", "observability", path}, &out, &errOut); code != 0 {
		t.Fatalf("profile deploy pull: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "deployment profile-stack: all 0 image reference(s)") {
		t.Fatalf("profile deploy pull did not use its selected service set: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "push", "--profile", "frontend", "unused:tag"}, &out, &errOut); code != 2 {
		t.Fatalf("image push unexpectedly accepted a deployment-only profile flag: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestDeployProfilesCommandListsDefaultAndOptInServices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	manifest := `{
		"version": 1,
		"name": "profile-inventory",
		"services": {
			"database": {
				"command": ["/bin/database"],
				"options": {"labels": {"classification": "private-label"}}
			},
			"web": {
				"command": ["/bin/web", "private-command"],
				"options": {"env": ["TOKEN=private-secret"]},
				"profiles": ["frontend", "diagnostics"]
			},
			"metrics": {
				"command": ["/bin/metrics"],
				"profiles": ["diagnostics"]
			}
		}
	}`
	if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"deploy", "profiles", "--json", path}, &out, &errOut); code != 0 {
		t.Fatalf("deploy profiles --json: code=%d stderr=%q", code, errOut.String())
	}
	var inventory deploy.ProfileInventory
	if err := json.Unmarshal(out.Bytes(), &inventory); err != nil {
		t.Fatalf("decode deploy profile inventory: %v", err)
	}
	want := deploy.ProfileInventory{
		DefaultServices: []string{"database"},
		Profiles: []deploy.ProfileGroup{
			{Name: "diagnostics", Services: []string{"metrics", "web"}},
			{Name: "frontend", Services: []string{"web"}},
		},
	}
	if !reflect.DeepEqual(inventory, want) {
		t.Fatalf("deploy profile inventory = %+v, want %+v", inventory, want)
	}
	for _, secret := range []string{"private-command", "private-secret", "private-label"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("profile inventory leaked %q: %s", secret, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deploy", "profiles", path}, &out, &errOut); code != 0 {
		t.Fatalf("deploy profiles text: code=%d stderr=%q", code, errOut.String())
	}
	for _, expected := range []string{"DEFAULT SERVICES", "database", "SERVICE PROFILES", "diagnostics", "frontend", "metrics, web"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("text profile inventory missing %q: %s", expected, out.String())
		}
	}
}

func TestDeploymentEnvironmentFilesMergeInDeclarationOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "base.env")
	second := filepath.Join(dir, "service.env")
	if err := os.WriteFile(first, []byte("SHARED=base\nFROM_BASE=one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("SHARED=service\nFROM_SERVICE=two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options, err := deploymentContainerOptions(deploy.Service{
		EnvFiles: []string{first, second},
		Options:  container.Options{Env: []string{"SHARED=explicit", "DIRECT=value"}},
	})
	if err != nil {
		t.Fatalf("deploymentContainerOptions(): %v", err)
	}
	got := make(map[string]string, len(options.Env))
	for _, entry := range options.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid merged environment entry %q", entry)
		}
		got[key] = value
	}
	want := map[string]string{
		"SHARED":       "explicit",
		"FROM_BASE":    "one",
		"FROM_SERVICE": "two",
		"DIRECT":       "value",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged environment = %#v, want %#v", got, want)
	}
}

func TestDeploymentEnvironmentFilesFailClosedWhenMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.env")
	_, err := deploymentContainerOptions(deploy.Service{
		EnvFiles: []string{missing},
	})
	if err == nil || !strings.Contains(err.Error(), "environment file 1 could not be read") {
		t.Fatalf("expected unreadable environment file to fail closed, got %v", err)
	}
}

func TestDeploymentServiceStartTimeoutOverridesStackDefault(t *testing.T) {
	fallback := 30 * time.Second
	tests := []struct {
		name    string
		service deploy.Service
		want    time.Duration
	}{
		{name: "inherit", service: deploy.Service{}, want: fallback},
		{name: "short override", service: deploy.Service{StartTimeoutMS: 1500}, want: 1500 * time.Millisecond},
		{name: "maximum override", service: deploy.Service{StartTimeoutMS: 600000}, want: 10 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deploymentServiceStartTimeout(test.service, fallback); got != test.want {
				t.Fatalf("deploymentServiceStartTimeout() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestDeploymentStopTimeoutUsesPersistedServiceOverride(t *testing.T) {
	fallback := 10 * time.Second
	tests := []struct {
		name      string
		labels    map[string]string
		want      time.Duration
		wantError bool
	}{
		{name: "inherit stack default", labels: map[string]string{}, want: fallback},
		{name: "service override", labels: map[string]string{deploy.StopTimeoutLabel: "1500"}, want: 1500 * time.Millisecond},
		{name: "minimum override", labels: map[string]string{deploy.StopTimeoutLabel: "10"}, want: 10 * time.Millisecond},
		{name: "maximum override", labels: map[string]string{deploy.StopTimeoutLabel: "600000"}, want: 10 * time.Minute},
		{name: "invalid integer", labels: map[string]string{deploy.StopTimeoutLabel: "slow"}, wantError: true},
		{name: "below minimum", labels: map[string]string{deploy.StopTimeoutLabel: "9"}, wantError: true},
		{name: "above maximum", labels: map[string]string{deploy.StopTimeoutLabel: "600001"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := deploymentStopTimeout(test.labels, fallback)
			if test.wantError {
				if err == nil {
					t.Fatal("invalid persisted timeout unexpectedly accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("deploymentStopTimeout(): %v", err)
			}
			if got != test.want {
				t.Fatalf("deploymentStopTimeout() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestDeploymentStopGraceChildProcess(t *testing.T) {
	if os.Getenv("BOXR_STOP_GRACE_CHILD") != "1" {
		return
	}
	signal.Ignore(os.Interrupt)
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	select {}
}

func TestStopDeploymentProcessesHonorsIndependentGracePeriods(t *testing.T) {
	startHelper := func(name string, timeout time.Duration) *deploymentProcess {
		t.Helper()
		command := exec.Command(os.Args[0], "-test.run=^TestDeploymentStopGraceChildProcess$")
		command.Env = append(os.Environ(), "BOXR_STOP_GRACE_CHILD=1")
		pipe, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(pipe).ReadString('\n')
		if err != nil || line != "ready\n" {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("stop helper did not become ready: line=%q err=%v", line, err)
		}
		return &deploymentProcess{service: name, stopTimeout: timeout, command: command}
	}

	short := startHelper("short", 50*time.Millisecond)
	long := startHelper("long", 220*time.Millisecond)
	exits := make(chan deploymentProcessExit, 2)
	for _, process := range []*deploymentProcess{short, long} {
		go func(process *deploymentProcess) {
			exits <- deploymentProcessExit{service: process.service, err: process.command.Wait()}
		}(process)
	}
	var out, errOut bytes.Buffer
	exited := make(map[string]deploymentProcessExit)
	started := time.Now()
	stopDeploymentProcesses([]*deploymentProcess{short, long}, exited, exits, 2*time.Second, &out, &errOut)
	elapsed := time.Since(started)
	if elapsed < 150*time.Millisecond || elapsed > time.Second {
		t.Fatalf("independent stop grace periods took %s; want roughly the longest 220ms service grace, not the 2s fallback", elapsed)
	}
	if len(exited) != 2 {
		t.Fatalf("stopped %d services, want both: %#v", len(exited), exited)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stop process errors: %s", errOut.String())
	}
}

func TestRunDryRunSupportsManagedBridgeNetwork(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{
		"run", "--dry-run", "--network", "bridge", "--network-name", "frontend",
		"/bin/echo", "bridge-ready",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("bridge dry-run returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("decode bridge dry-run plan: %v", err)
	}
	if plan.NetworkMode != "bridge" || plan.NetworkName != "frontend" || !plan.NoContainerStateMade {
		t.Fatalf("bridge dry-run plan = %+v", plan)
	}
}

func TestRunBridgeModeRequiresNetworkNameBeforeExecution(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--dry-run", "--network", "bridge", "/bin/true"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "requires --network-name") {
		t.Fatalf("bridge without network name returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestNetworkListCommandReturnsEmptyJSONWithoutHostMutation(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	var out, errOut bytes.Buffer
	code := run([]string{"network", "ls", "--json"}, &out, &errOut)
	if code != 0 || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("network ls --json returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("network ls wrote unexpected errors: %s", errOut.String())
	}
}

func TestNetworkCommandValidatesUsageAndUnknownActions(t *testing.T) {
	for _, args := range [][]string{{"network"}, {"network", "create"}, {"network", "inspect"}, {"network", "unknown"}} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != 2 {
			t.Errorf("run(%v) returned %d, want 2: stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
	}
}

func TestParsePortMappingSupportsHostBindingAndProtocol(t *testing.T) {
	tests := []struct {
		value string
		want  container.PortMapping
	}{
		{
			value: "8080:80",
			want:  container.PortMapping{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		},
		{
			value: "127.0.0.1:8443:443/tcp",
			want:  container.PortMapping{HostIP: "127.0.0.1", HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
		},
		{
			value: "5353:53/UDP",
			want:  container.PortMapping{HostIP: "0.0.0.0", HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
		},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := parsePortMapping(test.value)
			if err != nil {
				t.Fatalf("parsePortMapping(): %v", err)
			}
			if got != test.want {
				t.Fatalf("parsePortMapping() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParsePortMappingRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{
		"", "8080", "0:80", "8080:0", "65536:80", "8080:65536",
		"127.0.0.1:8080:80/sctp", "127.0.0.1:8080:80/tcp/extra",
		"::1:8080:80", " 8080:80", "8080:80 ",
	} {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			if _, err := parsePortMapping(value); err == nil {
				t.Fatalf("parsePortMapping(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestRunDryRunIncludesValidatedPublishedPorts(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{
		"run", "--dry-run", "--network", "bridge", "--network-name", "frontend",
		"--publish", "127.0.0.1:8080:80/tcp", "-p", "5353:53/udp", "--", "/bin/echo", "ready",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("published port dry-run returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("decode published port dry-run: %v", err)
	}
	want := []container.PortMapping{
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "0.0.0.0", HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
	}
	if !reflect.DeepEqual(plan.PublishedPorts, want) {
		t.Fatalf("published ports = %+v, want %+v", plan.PublishedPorts, want)
	}
	if !plan.NoContainerStateMade {
		t.Fatal("dry-run unexpectedly reports creating container state")
	}
}

func TestRunRejectsPublishedPortsOutsideBridgeMode(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--dry-run", "--publish", "8080:80", "/bin/true"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "require network mode bridge") {
		t.Fatalf("published port without bridge returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestNetworkConnectAttachesRunningPrivateNamespaceAndPublishedPorts(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("live-connect-api", []string{"/bin/service"}, container.Options{NetworkMode: "none"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	oldJoin, oldVerify := joinExistingContainerNetwork, verifyContainerNamespacePID
	t.Cleanup(func() {
		joinExistingContainerNetwork, verifyContainerNamespacePID = oldJoin, oldVerify
	})
	var gotNetwork, gotID string
	var gotPID int
	var gotPorts []container.PortMapping
	joinExistingContainerNetwork = func(network string, pid int, id string, ports ...container.PortMapping) (string, error) {
		gotNetwork, gotPID, gotID = network, pid, id
		gotPorts = append([]container.PortMapping(nil), ports...)
		return "172.30.10.8", nil
	}
	verifyContainerNamespacePID = func(pid int) error {
		if pid != os.Getpid() {
			t.Fatalf("verified PID = %d, want test process %d", pid, os.Getpid())
		}
		return nil
	}

	var out, errOut bytes.Buffer
	code := run([]string{
		"network", "connect", "--publish", "127.0.0.1:8080:80/tcp",
		"frontend", "live-connect-api",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("network connect returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	wantPorts := []container.PortMapping{{
		HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp",
	}}
	if gotNetwork != "frontend" || gotID != record.ID || gotPID != os.Getpid() || !reflect.DeepEqual(gotPorts, wantPorts) {
		t.Fatalf("network attach arguments = network %q id %q pid %d ports %+v", gotNetwork, gotID, gotPID, gotPorts)
	}
	if !strings.Contains(out.String(), "172.30.10.8") {
		t.Fatalf("connect output does not include allocated address: %q", out.String())
	}
}

func TestNetworkConnectRejectsHostNetworkContainerBeforeNamespaceEntry(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("live-connect-host", []string{"/bin/service"}, container.Options{NetworkMode: "host"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	oldJoin, oldVerify := joinExistingContainerNetwork, verifyContainerNamespacePID
	t.Cleanup(func() {
		joinExistingContainerNetwork, verifyContainerNamespacePID = oldJoin, oldVerify
	})
	verifyCalled := false
	joinCalled := false
	verifyContainerNamespacePID = func(int) error {
		verifyCalled = true
		return nil
	}
	joinExistingContainerNetwork = func(string, int, string, ...container.PortMapping) (string, error) {
		joinCalled = true
		return "172.30.10.8", nil
	}
	var out, errOut bytes.Buffer
	code := run([]string{"network", "connect", "frontend", "live-connect-host"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "started with --network none") {
		t.Fatalf("network connect host mode returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if verifyCalled || joinCalled {
		t.Fatalf("unsafe host-network container reached namespace attachment: verify=%v join=%v", verifyCalled, joinCalled)
	}
}

func TestNetworkDisconnectDetachesRunningPrivateNamespace(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("live-disconnect-api", []string{"/bin/service"}, container.Options{NetworkMode: "none"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	oldLeave := leaveExistingContainerNetwork
	t.Cleanup(func() { leaveExistingContainerNetwork = oldLeave })
	var gotNetwork, gotID string
	leaveExistingContainerNetwork = func(network, id string) error {
		gotNetwork, gotID = network, id
		return nil
	}
	var out, errOut bytes.Buffer
	code := run([]string{"network", "disconnect", "frontend", "live-disconnect-api"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("network disconnect returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if gotNetwork != "frontend" || gotID != record.ID || !strings.Contains(out.String(), "disconnected container") {
		t.Fatalf("network detach arguments/output = network %q id %q out %q", gotNetwork, gotID, out.String())
	}
}

func TestVerifyContainerRuntimePIDRejectsNonBoxrProcess(t *testing.T) {
	if err := verifyContainerRuntimePID(os.Getpid()); err == nil {
		t.Fatal("test process unexpectedly passed Boxr helper PID verification")
	}
}

func TestNetworkPruneDefaultsToDryRunAndKeepsLiveLeases(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := boxrnetwork.RunnerFunc(func(context.Context, string, ...string) error { return nil })
	if _, err := boxrnetwork.CreateWithRunner(context.Background(), "stale-net", boxrnetwork.CreateOptions{
		Subnet: "172.30.90.0/24",
	}, runner); err != nil {
		t.Fatal(err)
	}
	if _, err := boxrnetwork.JoinWithRunner(context.Background(), "stale-net", 5101, "stale-container", runner); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"network", "prune"}, &out, &errOut); code != 0 {
		t.Fatalf("network prune dry-run returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	for _, expected := range []string{"DRY RUN", "stale-net/stale-container", "172.30.90.2", "network prune --force"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("network prune dry-run missing %q: %s", expected, out.String())
		}
	}
	summary, err := boxrnetwork.Inspect("stale-net")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 1 {
		t.Fatalf("dry-run mutated IPAM leases: %+v", summary)
	}
}

func TestNetworkPruneForceInvokesOwnershipCheckedCleanup(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	t.Setenv("BOXR_NETWORK_DIR", t.TempDir())
	oldPrune := pruneManagedNetworkAttachments
	t.Cleanup(func() { pruneManagedNetworkAttachments = oldPrune })
	called := false
	pruneManagedNetworkAttachments = func(live map[string]struct{}) (boxrnetwork.PruneReport, error) {
		called = true
		if len(live) != 0 {
			t.Fatalf("unexpected live container set: %#v", live)
		}
		return boxrnetwork.PruneReport{AttachmentsRemoved: 2, AddressesReleased: 2, PublishedPortsRemoved: 3}, nil
	}
	var out, errOut bytes.Buffer
	code := run([]string{"network", "prune", "--force"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("network prune --force returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !called || !strings.Contains(out.String(), "removed attachments: 2") ||
		!strings.Contains(out.String(), "released IPv4 addresses: 2") ||
		!strings.Contains(out.String(), "removed published ports: 3") {
		t.Fatalf("prune result not reported: called=%v stdout=%q", called, out.String())
	}
}

func TestNetworkPruneRejectsConflictingModes(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"network", "prune", "--dry-run", "--force"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "Usage: boxr network prune") {
		t.Fatalf("conflicting prune modes returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestContainerNamespaceHelperAliveRejectsTestProcess(t *testing.T) {
	if containerNamespaceHelperAlive(os.Getpid()) {
		t.Fatal("ordinary test process was incorrectly counted as a live Boxr namespace helper")
	}
}

func TestRunDetachCannotBeCombinedWithDryRun(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--detach", "--dry-run", "/bin/true"},
		{"run", "-d", "--dry-run", "/bin/true"},
	} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), "--detach cannot be combined with --dry-run") {
			t.Errorf("run(%v) returned %d: stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
	}
}

func TestRunDetachFalseKeepsForegroundDryRun(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--detach=false", "--dry-run", "/bin/true"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run --detach=false --dry-run returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("decode foreground plan: %v", err)
	}
	if !plan.NoContainerStateMade || plan.Command != "/bin/true" {
		t.Fatalf("foreground dry-run plan = %+v", plan)
	}
}

func TestRunDoesNotTreatCommandArgumentAsDetachFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--dry-run", "--", "/bin/echo", "--detach"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run command argument --detach returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("decode command argument plan: %v", err)
	}
	if plan.Command != "/bin/echo" || plan.ArgumentCount != 1 {
		t.Fatalf("command argument was interpreted as a run option: %+v", plan)
	}
}

func TestRestartPolicyEvaluationHonorsExitStatusAndRetryBudget(t *testing.T) {
	failed := state.Record{Status: "failed", ExitCode: intPointer(7)}
	succeeded := state.Record{Status: "exited", ExitCode: intPointer(0)}
	tests := []struct {
		name    string
		options container.Options
		record  state.Record
		want    bool
	}{
		{name: "no policy", options: container.Options{RestartPolicy: "no"}, record: failed, want: false},
		{name: "always on success", options: container.Options{RestartPolicy: "always"}, record: succeeded, want: true},
		{name: "unless stopped on failure", options: container.Options{RestartPolicy: "unless-stopped"}, record: failed, want: true},
		{name: "on failure", options: container.Options{RestartPolicy: "on-failure"}, record: failed, want: true},
		{name: "on failure ignores success", options: container.Options{RestartPolicy: "on-failure"}, record: succeeded, want: false},
		{name: "unlimited on failure", options: container.Options{RestartPolicy: "on-failure"}, record: state.Record{ExitCode: intPointer(2), RestartCount: 1000}, want: true},
		{name: "retry budget available", options: container.Options{RestartPolicy: "on-failure", RestartMaxRetries: 3}, record: state.Record{ExitCode: intPointer(2), RestartCount: 2}, want: true},
		{name: "retry budget exhausted", options: container.Options{RestartPolicy: "on-failure", RestartMaxRetries: 3}, record: state.Record{ExitCode: intPointer(2), RestartCount: 3}, want: false},
		{name: "manual stop suppresses always", options: container.Options{RestartPolicy: "always"}, record: state.Record{ExitCode: intPointer(2), StopRequested: true}, want: false},
		{name: "manual stop suppresses unless stopped", options: container.Options{RestartPolicy: "unless-stopped"}, record: state.Record{ExitCode: intPointer(2), StopRequested: true}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := restartPolicyApplies(test.options, test.record); got != test.want {
				t.Fatalf("restartPolicyApplies() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWaitForContainerRestartCanBeCancelledByStopRequest(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("restart-delay-cancel", []string{"/bin/false"}, container.Options{RestartPolicy: "always"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 5401); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, &container.ExitError{Code: 1}); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRestart(record.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan bool, 1)
	go func() { result <- waitForContainerRestart(record.ID, 600*time.Millisecond) }()
	time.Sleep(150 * time.Millisecond)
	if err := state.RequestStop(record.ID); err != nil {
		t.Fatal(err)
	}
	if <-result {
		t.Fatal("restart delay continued after manual stop intent")
	}
	loaded, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "failed" || !loaded.StopRequested {
		t.Fatalf("cancelled restart record = %+v", loaded)
	}
}

func TestRestartPoliciesRequireDetachedRunsAndCannotAutoRemove(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--restart", "always", "/bin/true"},
		{"run", "--detach", "--restart", "on-failure", "--rm", "/bin/true"},
	} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != 2 {
			t.Errorf("run(%v) returned %d, want 2: stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
	}
}

func TestRunDryRunDescribesRestartPolicyAndDelay(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--dry-run", "--restart", "on-failure", "--restart-max-retries", "3", "--restart-delay", "1500ms", "/bin/true"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("restart dry-run returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var plan runPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.RestartPolicy != "on-failure" || plan.RestartMaxRetries != 3 || plan.RestartDelayMS != 1500 {
		t.Fatalf("restart dry-run plan = %+v", plan)
	}
}

func intPointer(value int) *int { return &value }

func TestRunStopCancelsContainerPendingRestart(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("stop-pending-restart", []string{"/bin/false"}, container.Options{RestartPolicy: "always"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 5501); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, &container.ExitError{Code: 1}); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRestart(record.ID); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"stop", record.ID}, &out, &errOut); code != 0 {
		t.Fatalf("stop pending restart returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	cancelled, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "failed" || !cancelled.StopRequested || !strings.Contains(out.String(), "cancelled pending restart") {
		t.Fatalf("stop did not cancel restart: record=%+v stdout=%q", cancelled, out.String())
	}
}

func TestRunKillTermCancelsContainerPendingRestart(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("kill-pending-restart", []string{"/bin/false"}, container.Options{RestartPolicy: "always"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Start(record.ID, 5502); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(record.ID, &container.ExitError{Code: 1}); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRestart(record.ID); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"kill", "--signal", "TERM", record.ID}, &out, &errOut); code != 0 {
		t.Fatalf("kill pending restart returned %d: stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	cancelled, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "failed" || !cancelled.StopRequested || !strings.Contains(out.String(), "cancelled pending restart") {
		t.Fatalf("kill did not cancel restart: record=%+v stdout=%q", cancelled, out.String())
	}
}

func TestDeferImageServiceBindMountsUsesRunFlagsAfterImageUnpack(t *testing.T) {
	options := container.Options{
		BindMounts: []container.BindMount{
			{Source: "/srv/boxr/data", Target: "/var/lib/app", ReadOnly: true},
			{Source: "/srv/boxr/config", Target: "/etc/app"},
		},
	}
	args, err := deferImageServiceBindMounts("example:latest", &options)
	if err != nil {
		t.Fatalf("defer image service mounts: %v", err)
	}
	want := []string{
		"--mount", "/srv/boxr/data:/var/lib/app:ro",
		"--mount", "/srv/boxr/config:/etc/app:rw",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("deferred mount args = %#v, want %#v", args, want)
	}
	if len(options.BindMounts) != 0 {
		t.Fatalf("bind mounts remain in config options: %#v", options.BindMounts)
	}
}

func TestDeferImageServiceBindMountsLeavesNonImageOptionsUntouched(t *testing.T) {
	options := container.Options{
		BindMounts: []container.BindMount{{Source: "/srv/data", Target: "/data"}},
	}
	args, err := deferImageServiceBindMounts("", &options)
	if err != nil {
		t.Fatalf("defer non-image mounts: %v", err)
	}
	if len(args) != 0 || len(options.BindMounts) != 1 {
		t.Fatalf("non-image mounts changed: args=%#v options=%+v", args, options)
	}
}

func TestDeferImageServiceBindMountsRejectsUnrepresentablePaths(t *testing.T) {
	for _, source := range []string{"/srv/data:with-colon", "/srv/data\\with-backslash", "/srv/data\x00invalid"} {
		options := container.Options{
			BindMounts: []container.BindMount{{Source: source, Target: "/data"}},
		}
		if _, err := deferImageServiceBindMounts("example:latest", &options); err == nil {
			t.Errorf("unrepresentable mount source %q was accepted", source)
		}
		if len(options.BindMounts) != 1 {
			t.Errorf("invalid mount source %q mutated options", source)
		}
	}
}

func TestCompletionIncludesDockerEngineDaemon(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCompletion([]string{"bash"}, &stdout, &stderr); code != 0 {
		t.Fatalf("bash completion returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "daemon") {
		t.Fatalf("completion does not include daemon command: %s", stdout.String())
	}
}
